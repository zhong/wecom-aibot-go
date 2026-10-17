package acp

import (
	"context"
	"fmt"
	"sync"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

// Router 把企微会话（chatid 或单聊 userid）路由到各自的 ACP agent
// 会话。每个企微会话独立一个 agent 会话，消息按会话串行：进行中的
// 下一轮 ask 会排队等待（对齐 pi-acp 的天然排队语义），不会并行交叠。
type Router struct {
	proc *agentProc
	bot  *wecomaibot.Client

	mu       sync.Mutex
	sessions map[string]*sessionEntry
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

// entry 取（或惰性创建）一个企微会话的路由条目。
func (r *Router) entry(chatKey string) (*sessionEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.sessions[chatKey]; ok {
		return e, nil
	}
	// session/new 期间持锁：并发首消息只建一个条目
	acpID, err := r.proc.initializeSession(context.Background())
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
	bot.OnText(func(ctx context.Context, m wecomaibot.TextMessage) {
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, m.Text.Content)
	})
	bot.OnVoice(func(ctx context.Context, m wecomaibot.VoiceMessage) {
		// 语音已被企微转写（VoiceContent 无下载 URL，只有转写文本）
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, m.Voice.Content)
	})
	bot.OnImage(func(ctx context.Context, m wecomaibot.ImageMessage) {
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, "[用户发来一张图片]")
	})
	bot.OnVideo(func(ctx context.Context, m wecomaibot.VideoMessage) {
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, "[用户发来一段视频]")
	})
	bot.OnFile(func(ctx context.Context, m wecomaibot.FileMessage) {
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, "[用户发来文件]")
	})
	bot.OnMixed(func(ctx context.Context, m wecomaibot.MixedMessage) {
		var sb []byte
		for _, item := range m.Mixed.MsgItem {
			if item.Text != nil && item.Text.Content != "" {
				sb = append(sb, item.Text.Content...)
				sb = append(sb, '\n')
			}
		}
		handle(ctx, chatKeyOf(m.BaseMessage, m.From.UserID), m, string(sb))
	})
}

// chatKeyOf 生成路由键：群聊用 ChatID；单聊没有 ChatID 时用 userid。
func chatKeyOf(base wecomaibot.BaseMessage, userID string) string {
	if base.ChatID != "" {
		return base.ChatID
	}
	return "dm:" + userID
}

// initializeSession 在已有 initialize 握手之后新建一个 agent 会话。
func (p *agentProc) initializeSession(ctx context.Context) (string, error) {
	return p.newSessionOnly(ctx)
}
