package acp

import (
	"context"
	"fmt"
	"sync"

	wecomaibot "github.com/zhong/wecom-aibot-go"
	"github.com/zhong/wecom-aibot-go/llm"
	"github.com/zhong/wecom-aibot-go/mcp"
)

// Router 把企微会话（chatid 或单聊 userid）路由到各自的 ACP agent
// 会话。每个企微会话独立一个 agent 会话，消息按会话串行：进行中的
// 下一轮 ask 会排队等待（对齐 pi-acp 的天然排队语义），不会并行交叠。
type Router struct {
	proc *agentProc
	bot  *wecomaibot.Client

	mu       sync.Mutex
	sessions map[string]*sessionEntry

	// 工具面（可选，EnableTools 启用）：工具执行端点 + 每会话 cwd 基目录。
	surface     mcp.ToolSurface // 由 toolsStateMu 保护
	toolWorkDir string
}

// sessionEntry 是一个企微会话的路由条目：session 桥接器 + 消息串行锁。
type sessionEntry struct {
	s  *session
	mu chan struct{} // ask 互斥：同时只有一个 prompt 在途
}

// NewRouter 创建路由器并用给定配置拉起 ACP 适配器子进程、完成
// initialize 握手。
func NewRouter(ctx context.Context, bot *wecomaibot.Client, cfg AgentConfig) (*Router, error) {
	proc, err := newAgentProc(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := proc.initialize(ctx); err != nil {
		return nil, err
	}
	return &Router{proc: proc, bot: bot, sessions: map[string]*sessionEntry{}}, nil
}

// entry 取（或惰性创建）一个企微会话的路由条目。启用工具面时，每个新
// 会话各建一个专属 cwd 目录（其 .pi/mcp.json 指向会话专属 MCP 端点，
// 由 enableSessionTools 完成）。返回的条目携带该会话的 agent cwd
// （工具面附件落盘目录；未启用时为空）。

// sessionCwd 返回该会话 agent 的 cwd：启用工具面时是 workDir 下的专属
// 目录（附件落盘/项目配置都在这里），否则空。
func (r *Router) sessionCwd(chatKey string) string {
	surface, workDir := r.toolSurface()
	if surface == nil {
		return ""
	}
	return sessionWorkDir(workDir, chatKey)
}

func (r *Router) entry(chatKey string) (*sessionEntry, error) {
	// 工具面状态惰性快照：entry 持 Router.mu 串行读，Handle 不会与
	// EnableTools 并发（EnableTools 须在首条消息前调用，文档已注明）。
	surface, workDir := r.toolSurface()
	if surface != nil {
		if err := r.enableSessionTools(chatKey, surface, workDir); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sessions[chatKey]; ok {
		return e, nil
	}
	// session/new 期间持锁：并发首消息只建一个条目
	cwd := "/tmp"
	if surface != nil {
		cwd = sessionWorkDir(workDir, chatKey)
	}
	acpID, err := r.proc.newSessionOnly(context.Background(), cwd)
	if err != nil {
		return nil, err
	}
	s := &session{proc: r.proc, bot: r.bot, acpID: acpID}
	r.proc.registerUpdate(acpID, s.handleUpdate)
	e := &sessionEntry{s: s, mu: make(chan struct{}, 1)}
	r.sessions[chatKey] = e
	return e, nil
}

// Handle 处理一条企微消息：路由到对应会话，串行 ask，返回终帧正文。
// req 为消息载体（流式帧 req_id / ChatID 的来源），userText 是给 agent
// 的用户消息文本，streamID 是本轮企微流式回复的流 ID。
func (r *Router) Handle(ctx context.Context, chatKey string, req wecomaibot.RequestCarrier, userText, streamID string) (string, error) {
	e, err := r.entry(chatKey)
	if err != nil {
		return "", fmt.Errorf("建立 agent 会话失败: %w", err)
	}
	// 串行：进行中的 ask 未收尾时，本条消息排队（不丢弃、不并行交叠）
	e.mu <- struct{}{}
	defer func() { <-e.mu }()

	// 工具面：prompt 在途期间把消息载体快照登进 MCP 端点（agent 由此连到
	// 本会话的执行上下文；saveDir 传会话 cwd，附件落盘写到 agent 能读的地方）。
	// defer 在释放串行锁前清除（LIFO），同会话的下一条消息不可能读到别的快照。
	if surface, _ := r.toolSurface(); surface != nil {
		surface.SetChat(chatKey, req, r.sessionCwd(chatKey))
		defer surface.ClearChat(chatKey)
	}

	e.s.begin(req, streamID)
	return e.s.ask(ctx, userText), nil
}

// RouterBind 把 Router 绑到 Client 的文本/语音/图文等消息回调上，
// 自动完成 chatKey 提取与 prompt 文本组装。streamIDPrefix 用于
// GenerateReqID 生成每轮流式帧 ID。
//
// 返回的解绑函数目前为 nil（回调注册在 Client 上无解绑面）。
func RouterBind(bot *wecomaibot.Client, r *Router) {
	handle := func(ctx context.Context, chatKey string, req wecomaibot.RequestCarrier, userText string) {
		streamID := wecomaibot.GenerateReqID("acp")
		if _, err := r.Handle(ctx, chatKey, req, userText, streamID); err != nil {
			// 建会话失败：终帧降级文案直接告知用户
			if chatID := extractChatID(req); chatID != "" {
				_, _ = bot.SendMessage(chatID, map[string]any{
					"msgtype":  "markdown",
					"markdown": map[string]any{"content": DefaultErrorText},
				})
			}
		}
	}
	// saveInflight 已从闭包提为 Router 方法（见下）：测试直打该路径。
	bot.OnText(func(ctx context.Context, m wecomaibot.TextMessage) {
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, textWithQuoteHint(m.Text.Content, m.Quote))
	})
	bot.OnVoice(func(ctx context.Context, m wecomaibot.VoiceMessage) {
		// 语音已被企微转写（VoiceContent 无下载 URL，只有转写文本）
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, m.Voice.Content)
	})
	bot.OnImage(func(ctx context.Context, m wecomaibot.ImageMessage) {
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, "[用户发来一张图片]"+r.saveInflight(m))
	})
	bot.OnVideo(func(ctx context.Context, m wecomaibot.VideoMessage) {
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, "[用户发来一段视频]"+r.saveInflight(m))
	})
	bot.OnFile(func(ctx context.Context, m wecomaibot.FileMessage) {
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, "[用户发来文件]"+r.saveInflight(m))
	})
	bot.OnMixed(func(ctx context.Context, m wecomaibot.MixedMessage) {
		// 图文混排：正文文本进 prompt，附件走与直发消息同路径的落盘
		// （llm.attachmentOf 支持从 MixedMsgItem 提取附件——不落盘的话
		// 混排里的图既读不到也无提示，静默丢失）。
		var sb []byte
		for _, item := range m.Mixed.MsgItem {
			if item.Text != nil && item.Text.Content != "" {
				sb = append(sb, item.Text.Content...)
				sb = append(sb, '\n')
			}
		}
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, string(sb)+r.saveInflight(m))
	})
}

// saveInflight 把在途附件落盘到该会话 agent 的 cwd，返回 prompt 后缀：
// pi-acp 0.0.34 的 RPC 模式不加载 MCP 客户端（模型知道工具名但手
// 上没有可调用的工具），不落盘的话模型只能满文件系统盲找。落盘失败
// 或工具面未启用（cwd 为空）时退回 read_attachment 引导（本机 MCP
// 客户端形态仍可用）。
func (r *Router) saveInflight(req wecomaibot.RequestCarrier) string {
	chatKey := chatKeyOfReq(req)
	cwd := r.sessionCwd(chatKey)
	if cwd == "" {
		return attachmentHint
	}
	path, err := llm.SaveInflightAttachment(context.Background(), r.bot, req, cwd)
	if err != nil || path == "" {
		return attachmentHint
	}
	return "\n[附件已保存到 " + path + "，可直接读取该文件]"
}

// attachmentHint 是直发附件消息的 prompt 后缀：直发的图片/文件/视频
// 只带占位正文（"[用户发来文件]"），不提示的话 agent 会在文件系统里
// 盲找落盘文件（实测踩坑）。附件本体由 read_attachment 工具从消息
// 载体按需读取——工具面未启用时该提示无效但不致误导（agent 会在会话
// 历史里看到同样的提示句历史，回复里说明无工具即可）。
const attachmentHint = "\n[附件本体经 read_attachment 工具读取，先调它拿到内容]"

// textWithQuoteHint 组装文本消息的 prompt：群聊里独立图片/文件不产生
// 回调，用户会以「引用附件 @机器人」的方式送达附件——给 agent 一句提示，
// 附件本体由 read_attachment 工具读取；引用纯文本/无引用不加提示。
func textWithQuoteHint(content string, quote *wecomaibot.QuoteContent) string {
	if quote == nil {
		return content
	}
	if quote.Image != nil || quote.File != nil || quote.Mixed != nil {
		return content + "\n[用户引用了一个附件，可用 read_attachment 读取]"
	}
	return content
}

// chatKeyOf 生成路由键：群聊用 ChatID；单聊没有 ChatID 时用 userid。
func chatKeyOf(base wecomaibot.BaseMessage, userID string) string {
	if base.ChatID != "" {
		return base.ChatID
	}
	return "dm:" + userID
}

// chatKeyOfReq 从任意消息载体生成路由键（与 chatKeyOf 同语义）。
func chatKeyOfReq(req wecomaibot.RequestCarrier) string {
	switch m := req.(type) {
	case wecomaibot.TextMessage:
		return chatKeyOf(m.BaseMessage, m.From.UserID)
	case wecomaibot.VoiceMessage:
		return chatKeyOf(m.BaseMessage, m.From.UserID)
	case wecomaibot.ImageMessage:
		return chatKeyOf(m.BaseMessage, m.From.UserID)
	case wecomaibot.VideoMessage:
		return chatKeyOf(m.BaseMessage, m.From.UserID)
	case wecomaibot.FileMessage:
		return chatKeyOf(m.BaseMessage, m.From.UserID)
	case wecomaibot.MixedMessage:
		return chatKeyOf(m.BaseMessage, m.From.UserID)
	}
	return ""
}
