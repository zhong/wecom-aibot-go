package acp

import (
	"context"
	"strings"
	"sync"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

// DefaultPlaceholder 是 prompt 在途期间先发给用户的占位帧文案。
const DefaultPlaceholder = "正在思考..."

// DefaultErrorText 是 agent 超时或出错时的兜底文案（对齐 llm 包语义）。
const DefaultErrorText = "抱歉，智能体服务暂时不可用，请稍后重试。"

// session 绑定一个企微会话与它的 ACP agent 会话：agent 的
// agent_message_chunk 增量在这里累积并通过 streamBridge 推成企微
// 流式帧，arkin 语义与 llm 包一致（占位 → 非阻塞累积 → 终帧闭环）。
type session struct {
	proc  *agentProc
	bot   *wecomaibot.Client
	acpID string // ACP sessionId

	mu sync.Mutex
	// req 当前消息载体（流式帧 req_id 的来源）；streamID 本轮流式帧 ID
	req      wecomaibot.RequestCarrier
	streamID string
	// content 已累积正文；sawPlaceholder 已发占位帧
	content        string
	sawPlaceholder bool
	// prompting prompt 在途标位：仅此期间收到的 chunk 才计入正文
	// （pi-acp 等适配器在 session/new 后会推送 startup info 块，
	// 必须按协议时机过滤，不能按内容猜。）
	prompting bool
}

// handleUpdate 处理一条 session/update 通知：只关心 agent_message_chunk。
func (s *session) handleUpdate(raw map[string]any) {
	params, _ := raw["params"].(map[string]any)
	update, _ := params["update"].(map[string]any)
	if update == nil || update["sessionUpdate"] != "agent_message_chunk" {
		return
	}
	text := chunkText(update)
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.prompting {
		// prompt 外的 chunk（startup info、排队通知等系统文本）不入正文
		return
	}
	if !s.sawPlaceholder && s.req != nil {
		_, _ = s.bot.ReplyStreamByID(s.req, s.streamID, DefaultPlaceholder, false, nil, nil)
		s.sawPlaceholder = true
	}
	s.content += text
	if s.req == nil {
		return
	}
	_, _, _ = s.bot.ReplyStreamNonBlocking(s.req, s.streamID, s.content, false, nil, nil)
}

// chunkText 抽取 chunk 的文本。content 在 v1 schema 里是数组；pi-acp 等
// 适配器实际发单对象——两种形状都兼容。
func chunkText(update map[string]any) string {
	switch content := update["content"].(type) {
	case map[string]any:
		if content["type"] == "text" {
			return textOf(content)
		}
	case []any:
		var sb strings.Builder
		for _, item := range content {
			if m, ok := item.(map[string]any); ok {
				sb.WriteString(textOf(m))
			}
		}
		return sb.String()
	}
	return ""
}

// textOf 返回 text 类内容块里的文本；非 text 块返回空串。
func textOf(block map[string]any) string {
	if block["type"] != "text" {
		return ""
	}
	t, _ := block["text"].(string)
	return t
}

// ask 发送一条用户消息并等待 agent 收尾，随后发企微终帧闭环。
// 返回 AgentReply 式语义的 finalText（终帧实际发出的全部正文字符）。
func (s *session) ask(ctx context.Context, userText string) string {
	s.mu.Lock()
	s.prompting = true
	s.mu.Unlock()

	stop, err := s.proc.prompt(ctx, s.acpID, userText)

	s.mu.Lock()
	s.prompting = false
	final := s.content
	req := s.req
	s.mu.Unlock()

	if final == "" {
		if err != nil {
			final = DefaultErrorText
		} else {
			final = "（智能体没有返回内容）"
		}
	}
	if req != nil {
		if _, ferr := s.bot.ReplyStreamByID(req, s.streamID, final, true, nil, nil); ferr != nil {
			// 终帧失败降级 SendMessage（对齐 llm 包 streamBridge.finish）
			if chatID := extractChatID(req); chatID != "" {
				_, _ = s.bot.SendMessage(chatID, map[string]any{
					"msgtype":  "markdown",
					"markdown": map[string]any{"content": DefaultErrorText},
				})
			}
		}
	}
	_ = stop
	return final
}

// begin 为新一轮企微消息重置会话状态（新载体、新流、正文清零）。
func (s *session) begin(req wecomaibot.RequestCarrier, streamID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.req = req
	s.streamID = streamID
	s.content = ""
	s.sawPlaceholder = false
}

// extractChatID 从消息载体提取 ChatID（终帧失败的降级发送目标）；
// 单聊消息可能没有 ChatID，返回空串表示无法降级。与 llm 包同语义。
func extractChatID(req wecomaibot.RequestCarrier) string {
	switch m := req.(type) {
	case wecomaibot.TextMessage:
		return m.ChatID
	case wecomaibot.ImageMessage:
		return m.ChatID
	case wecomaibot.VoiceMessage:
		return m.ChatID
	case wecomaibot.VideoMessage:
		return m.ChatID
	case wecomaibot.FileMessage:
		return m.ChatID
	case wecomaibot.MixedMessage:
		return m.ChatID
	case wecomaibot.EventMessage:
		return m.ChatID
	}
	return ""
}
