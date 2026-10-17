// llm-bot 是企业微信智能机器人对接大模型的完整生产示例：
// 文本/语音消息转发给 LLM（OpenAI 兼容或 Anthropic Claude），
// 模型增量实时推成企微流式回复（打字机效果），并维护每个会话的有界对话历史。
//
// 开启 LLM_TOOLS（默认开启）时向模型注册企微交互工具（发模板卡片、发
// markdown、读取用户附件），由模型按内容形态自主决定何时调用——通知/数据/
// 交互用卡片、格式化长文用 markdown、日常对话直接文本、附件先读再答；
// 工具执行的目标会话由执行器闭包绑定当前消息，模型无法伪造投递目标。
//
// 环境变量：
//
//	WECOM_BOT_ID       机器人 ID（必填）
//	WECOM_BOT_SECRET   机器人 Secret（必填）
//	LLM_PROVIDER       openai | anthropic（必填）
//	LLM_API_KEY        LLM API Key（必填）
//	LLM_BASE_URL       自定义端点/网关地址（可选，OpenAI 兼容网关如 GLM/DeepSeek 填这里）
//	LLM_MODEL          模型名（必填），如 gpt-4o / deepseek-chat / claude-sonnet-5-5
//	LLM_SYSTEM_PROMPT  系统提示词（可选）
//	LLM_HISTORY_LIMIT  每会话保留的最大消息条数（可选，默认 20）
//	LLM_INCLUDE_REASONING  置 1 时透传推理模型的思维链增量（可选，默认只发正文）
//	LLM_TOOLS          置 0 关闭工具调用（可选，默认开启）
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	wecomaibot "github.com/zhong/wecom-aibot-go"
	"github.com/zhong/wecom-aibot-go/llm"
)

func main() {
	botID := os.Getenv("WECOM_BOT_ID")
	secret := os.Getenv("WECOM_BOT_SECRET")
	if botID == "" || secret == "" {
		log.Fatal("请先设置环境变量 WECOM_BOT_ID 和 WECOM_BOT_SECRET")
	}

	client, err := newLLMClient()
	if err != nil {
		log.Fatalf("初始化 LLM 客户端失败: %v", err)
	}

	// 系统提示词：未配置时用具默认选型指引——模型按内容形态自主选择
	// 交互形式，不依赖用户显式指令。
	systemPrompt := os.Getenv("LLM_SYSTEM_PROMPT")
	if systemPrompt == "" {
		systemPrompt = defaultSystemPrompt
	}
	tools := toolsEnabled()
	history := newChatHistory(historyLimit())

	bot, err := wecomaibot.NewClient(wecomaibot.Config{
		BotID:                  botID,
		Secret:                 secret,
		MaxReconnectAttempts:   -1,
		MaxAuthFailureAttempts: -1, // 认证失败也无限重试（与重连预算独立）
		ReconnectIntervalMS:    1000,
		HeartbeatIntervalMS:    30000,
	})
	if err != nil {
		log.Fatalf("创建客户端失败: %v", err)
	}

	registerLifecycleLogs(bot)
	registerLLMHandlers(bot, client, systemPrompt, tools, history)
	startHealthServer(bot)

	if err := bot.Connect(); err != nil {
		log.Fatalf("连接失败: %v", err)
	}

	log.Println("LLM 机器人已启动，等待消息")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("收到退出信号，开始优雅停机")
	bot.Disconnect()
	log.Println("机器人已退出")
}

// newLLMClient 按 LLM_PROVIDER 构造对应的流式对话客户端。
func newLLMClient() (llm.Client, error) {
	provider := os.Getenv("LLM_PROVIDER")
	apiKey := os.Getenv("LLM_API_KEY")
	model := os.Getenv("LLM_MODEL")
	baseURL := os.Getenv("LLM_BASE_URL")

	switch provider {
	case "openai":
		opts := llm.OpenAIOptions{APIKey: apiKey, Model: model, IncludeReasoning: os.Getenv("LLM_INCLUDE_REASONING") == "1"}
		if baseURL != "" {
			opts.BaseURL = baseURL
		}
		return llm.NewOpenAIClient(opts)
	case "anthropic":
		opts := llm.AnthropicOptions{APIKey: apiKey, Model: model}
		if baseURL != "" {
			opts.BaseURL = baseURL
		}
		return llm.NewAnthropicClient(opts)
	default:
		log.Fatalf("LLM_PROVIDER 必须是 openai 或 anthropic，当前: %q", provider)
		return nil, nil
	}
}

func registerLifecycleLogs(bot *wecomaibot.Client) {
	bot.OnConnected(func(ctx context.Context) {
		log.Println("WebSocket 已连接")
	})
	bot.OnAuthenticated(func(ctx context.Context) {
		log.Println("认证成功")
	})
	bot.OnDisconnected(func(ctx context.Context, reason string) {
		log.Printf("连接断开: %s", reason)
	})
	bot.OnReconnecting(func(ctx context.Context, attempt int) {
		log.Printf("触发重连，第 %d 次", attempt)
	})
	bot.OnError(func(ctx context.Context, err error) {
		log.Printf("SDK 错误: %v", err)
	})
}

// chatHistory 维护每个会话的有界对话历史（Lite 简单内存实现，重启即失）。
type chatHistory struct {
	mu    sync.Mutex
	limit int
	store map[string][]llm.ChatMessage
}

func newChatHistory(limit int) *chatHistory {
	if limit <= 0 {
		limit = 20
	}
	return &chatHistory{limit: limit, store: make(map[string][]llm.ChatMessage)}
}

// append 追加一轮 user/assistant 并裁剪到 limit 条（保留最近的完整对话轮次）。
func (h *chatHistory) append(chatKey string, user, assistant string) []llm.ChatMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	msgs := append(h.store[chatKey],
		llm.ChatMessage{Role: "user", Content: user},
		llm.ChatMessage{Role: "assistant", Content: assistant},
	)
	// 按 user 开头裁剪，避免把 assistant 留在开头
	for len(msgs) > h.limit {
		if len(msgs) >= 2 && msgs[0].Role == "user" && msgs[1].Role == "assistant" {
			msgs = msgs[2:]
		} else {
			msgs = msgs[1:]
		}
	}
	h.store[chatKey] = msgs
	return msgs
}

// snapshot 返回chatKey 当前历史（供发起请求时拼上文）。
func (h *chatHistory) snapshot(chatKey string) []llm.ChatMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]llm.ChatMessage(nil), h.store[chatKey]...)
}

// defaultSystemPrompt 是未配置 LLM_SYSTEM_PROMPT 时的默认系统提示词：
// 引导模型按内容形态自主选择企微交互形式，而非只响应用户的明确指令。
const defaultSystemPrompt = `你是企业微信里的智能机器人助手。根据内容形态自主选择最合适的回复方式：
- 通知、公告、数据展示、需要用户选择或操作的内容 → 调用 send_template_card 发模板卡片；
- 大段格式化长文本（表格、多层列表、多级标题、代码块）→ 调用 send_markdown 补充发送；
- 日常对话、简短问答 → 直接写在普通回复文本里，不用任何工具；
- 用户消息携带附件（文件/图片/视频/图文混排）→ 先调用 read_attachment 读取内容再作答。
没有明确要求时也应主动选择更合适的呈现形式。发完卡片或 markdown 后，普通回复里给一句简短说明即可，不要重复其内容。`

func registerLLMHandlers(bot *wecomaibot.Client, client llm.Client, systemPrompt string, tools bool, history *chatHistory) {
	// 工具目录在开启时下发给模型；卡片/markdown 的目标会话由执行器闭包
	// 绑定到当前消息回调，模型只能决定内容。
	var toolDefs []llm.ToolDef
	if tools {
		toolDefs = llm.WeComToolDefs()
	}

	// 文本与语音（语音已被企微转写为文本）共用同一条 LLM Agent 回复链路
	handle := func(kind, chatKey, text string, req wecomaibot.RequestCarrier) {
		start := time.Now()
		log.Printf("[%s] 收到消息(chat=%s): %q", kind, chatKey, text)

		// 拼装本轮请求：历史快照（只读） + 本轮 user 消息
		chatReq := llm.ChatRequest{
			Messages:     append(history.snapshot(chatKey), llm.ChatMessage{Role: "user", Content: text}),
			SystemPrompt: systemPrompt,
			Tools:        toolDefs,
		}
		streamID := wecomaibot.GenerateReqID("stream")
		// FinalRoundOnly 开启终帧净化：终帧只携带最后一轮正文，
		// 工具轮的中间独白不进终稿与历史。
		final, err := llm.AgentReply(context.Background(), bot, req, streamID, client, chatReq,
			llm.NewWeComExecutor(bot, req), "正在思考...", llm.TurnOptions{FinalRoundOnly: true})
		if err != nil {
			log.Printf("[%s] LLM Agent 回复失败(耗时=%v): %v", kind, time.Since(start), err)
		}
		// 用终帧实际发出的文本补记这一轮：工具轮（卡片/markdown 操作）在
		// RunTurn 内即逝、不进历史；流水正文与用户所见保持一致。
		history.append(chatKey, text, final)
		log.Printf("[%s] 回复完成(耗时=%v, %d字): %q", kind, time.Since(start), len([]rune(final)), final)
	}

	bot.OnText(func(_ context.Context, msg wecomaibot.TextMessage) {
		safeGo("LLM 文本处理", func() {
			handle("text", chatKeyOf(msg.ChatID, msg.From.UserID), msg.Text.Content, msg)
		})
	})

	bot.OnVoice(func(_ context.Context, msg wecomaibot.VoiceMessage) {
		safeGo("LLM 语音处理", func() {
			handle("voice", chatKeyOf(msg.ChatID, msg.From.UserID), msg.Voice.Content, msg)
		})
	})

	// 附件类消息：文本描述进上下文，模型可用 read_attachment 工具
	// 自主读取附件内容（文本类附件会回喂内容预览）再作答。
	bot.OnFile(func(_ context.Context, msg wecomaibot.FileMessage) {
		safeGo("LLM 文件处理", func() {
			handle("file", chatKeyOf(msg.ChatID, msg.From.UserID), "[用户发来文件]", msg)
		})
	})
	bot.OnImage(func(_ context.Context, msg wecomaibot.ImageMessage) {
		safeGo("LLM 图片处理", func() {
			handle("image", chatKeyOf(msg.ChatID, msg.From.UserID), "[用户发来图片]", msg)
		})
	})
	bot.OnVideo(func(_ context.Context, msg wecomaibot.VideoMessage) {
		safeGo("LLM 视频处理", func() {
			handle("video", chatKeyOf(msg.ChatID, msg.From.UserID), "[用户发来视频]", msg)
		})
	})
	bot.OnMixed(func(_ context.Context, msg wecomaibot.MixedMessage) {
		safeGo("LLM 混排消息处理", func() {
			handle("mixed", chatKeyOf(msg.ChatID, msg.From.UserID), "[用户发来图文混排消息]", msg)
		})
	})

	bot.OnEnterChat(func(_ context.Context, msg wecomaibot.EventMessage) {
		safeGo("欢迎语", func() {
			_, err := bot.ReplyWelcome(msg, map[string]any{
				"msgtype": "text",
				"text":    map[string]any{"content": "您好，我是接入大模型的智能机器人，直接提问即可。"},
			})
			if err != nil {
				log.Printf("发送欢迎语失败: %v", err)
			}
		})
	})

	// 卡片交互事件：更新已有卡片必须在事件后 5 秒窗口内完成，一次 LLM 往返
	// 不可达——所以卡片更新走确定性路径，不经过 LLM。这里给出最小示例：
	// 按钮/投票点击后把选中结果回显到卡片。
	bot.OnTemplateCardEvent(func(_ context.Context, msg wecomaibot.EventMessage) {
		safeGo("卡片事件", func() {
			ev := msg.Event.TemplateCardEvent
			if ev == nil {
				return
			}
			log.Printf("[卡片事件] card_type=%s event_key=%q task_id=%q", ev.CardType, ev.EventKey, ev.TaskID)
			// 业务在此按 event_key 分发：如把投票计数落库、用 UpdateTemplateCard
			// 在窗口内回写更新后的卡片（空 userIDs 表示更新所有可见用户）。
			updated := wecomaibot.TemplateCard{
				"card_type":  wecomaibot.TemplateCardTypeMultipleInteraction,
				"task_id":    ev.TaskID,
				"main_title": map[string]any{"title": "已收到你的选择", "desc": ev.EventKey},
			}
			if _, err := bot.UpdateTemplateCard(msg, updated, nil); err != nil {
				log.Printf("更新卡片失败: %v", err)
			}
		})
	})
}

// toolsEnabled 读 LLM_TOOLS：默认开启，置 0 关闭工具调用。
func toolsEnabled() bool {
	return os.Getenv("LLM_TOOLS") != "0"
}

// chatKeyOf 生成会话 key：群聊用 ChatID；单聊没有 ChatID 时回退到 UserID。
func chatKeyOf(chatID, userID string) string {
	if chatID != "" {
		return chatID
	}
	return "user:" + userID
}

// historyLimit 读 LLM_HISTORY_LIMIT 环境变量，非法值回落 20。
func historyLimit() int {
	if s := os.Getenv("LLM_HISTORY_LIMIT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
		log.Printf("LLM_HISTORY_LIMIT 值不合法(%q)，回落默认 20", s)
	}
	return 20
}

func startHealthServer(bot *wecomaibot.Client) {
	// 半开连接从「真死」到「心跳判死」约有 2×心跳间隔的僵尸窗口，
	// healthz 额外用「最近收到数据时间」兜底（对齐 examples/production）。
	const livenessWindow = 2*30*time.Second + 10*time.Second

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		last := bot.LastReceivedAt()
		alive := bot.IsConnected() && (last.IsZero() || time.Since(last) < livenessWindow)
		if alive {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("disconnected"))
	})

	go func() {
		if err := http.ListenAndServe(":8080", mux); err != nil {
			log.Printf("健康检查服务退出: %v", err)
		}
	}()
}

func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("%s发生 panic: %v", name, rec)
			}
		}()
		fn()
	}()
}
