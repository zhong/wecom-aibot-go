// llm-bot 是企业微信智能机器人对接大模型的完整生产示例：
// 文本/语音消息转发给 LLM（OpenAI 兼容或 Anthropic Claude），
// 模型增量实时推成企微流式回复（打字机效果），并维护每个会话的有界对话历史。
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

	systemPrompt := os.Getenv("LLM_SYSTEM_PROMPT")
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
	registerLLMHandlers(bot, client, systemPrompt, history)
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

func registerLLMHandlers(bot *wecomaibot.Client, client llm.Client, systemPrompt string, history *chatHistory) {
	// 文本与语音（语音已被企微转写为文本）共用同一条 LLM 流式回复链路
	handle := func(kind, chatKey, text string, req wecomaibot.RequestCarrier) {
		start := time.Now()
		log.Printf("[%s] 收到消息(chat=%s): %q", kind, chatKey, text)

		// 拼装本轮请求：历史快照（只读） + 本轮 user 消息
		chatReq := llm.ChatRequest{
			Messages:     append(history.snapshot(chatKey), llm.ChatMessage{Role: "user", Content: text}),
			SystemPrompt: systemPrompt,
		}
		streamID := wecomaibot.GenerateReqID("stream")
		final, err := llm.StreamReply(context.Background(), bot, req, streamID, client, chatReq, "正在思考...")
		if err != nil {
			log.Printf("[%s] LLM 流式回复失败(耗时=%v): %v", kind, time.Since(start), err)
		}
		// 用终帧实际发出的文本补记这一轮，保持历史与用户所见一致
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
