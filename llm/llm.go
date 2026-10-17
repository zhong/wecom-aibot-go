// Package llm 提供与企业微信智能机器人 SDK 配套的大模型对接能力：
// 统一的流式对话抽象 Client、OpenAI 兼容与 Anthropic Claude 两种实现、
// 把 LLM 增量桥接到企微流式回复帧的 StreamReply，以及基于工具调用
// （Tool Calling）让模型自主调用企微丰富交互能力（模板卡片、markdown 等）
// 的 AgentReply 与 RunTurn。
//
// 本包只依赖 Go 标准库（net/http），不引入任何第三方 SDK；
// 单向依赖根包（wecomaibot），根包不感知本包。
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// trimSSEData 去掉 SSE 行的 "data:" 前缀与首尾空白；非 data 行返回空串。
func trimSSEData(line string) string {
	if !strings.HasPrefix(line, "data:") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "data:"))
}

// DefaultTimeoutMS 是 LLM 流式请求的整体默认超时（毫秒），
// 命名与根包 Config 的 XxxMS / DefaultXxx 约定保持一致。
const DefaultTimeoutMS = 120000

// ChatMessage 是一条对话消息。Role 取 "system" / "user" / "assistant" / "tool"：
// assistant 消息可携带 ToolCalls（模型发起的工具调用，与正文 Content 可并存）；
// role 为 "tool" 的消息用 ToolCallID 引用对应的调用并填写执行结果 Content。
type ChatMessage struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall // 仅 assistant：模型发起的工具调用
	ToolCallID string     // 仅 role "tool"：对应的 ToolCall.ID
}

// ToolCall 是一次已完成的工具调用（厂商无关）：ID 由模型生成、回传工具
// 结果时引用；Arguments 是分片拼接完成后的完整参数 JSON。
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// normalizeToolArgs 归一工具参数：空/空白参数兜底为 "{}"，保证调用方能直接解析。
func normalizeToolArgs(raw json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

// ToolDef 是一个可被模型调用的工具定义（厂商无关）。Parameters 是 JSON Schema。
type ToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// StreamEventType 是流式事件的种类：文本增量或一个拼接完成的工具调用。
type StreamEventType int

const (
	// StreamEventTextDelta 是一段文本增量（TextDelta 携带内容）。
	StreamEventTextDelta StreamEventType = iota
	// StreamEventToolCall 是一个参数累积完成的工具调用（ToolCall 携带内容）。
	StreamEventToolCall
)

// StreamEvent 是流式过程中的一个事件：要么是文本增量，要么是拼接完成的工具调用。
type StreamEvent struct {
	Type      StreamEventType
	TextDelta string   // Type == StreamEventTextDelta 时非空
	ToolCall  ToolCall // Type == StreamEventToolCall 时有效
}

// ChatRequest 是一次与厂商无关的流式对话请求。
type ChatRequest struct {
	Model        string // 必填
	Messages     []ChatMessage
	SystemPrompt string   // 非空时作为 system 注入：OpenAI 拼到 messages 头部，Anthropic 用顶层 system 字段
	Temperature  *float64 // nil 表示用服务端默认
	MaxTokens    int      // 0 表示用服务端默认；Anthropic 端点 max_tokens 必填，客户端兜底 1024
	// Tools 是可用的工具目录；为空表示本轮不开工具。
	// 非空时 OpenAI 实现同时下发 tool_choice:"auto"，由模型自行决定是否调用。
	Tools []ToolDef
}

// Client 是流式对话客户端的统一抽象：每个内容增量回调一次 onDelta，
// 内容由调用方累积。onDelta 返回 error 可提前中止流式读取并向上传播。
type Client interface {
	// StreamChat 发起一次流式对话，返回结束原因（如 "stop" / "length"，供观测）
	// 或错误。ctx 取消会中断底层 HTTP 请求。
	StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string) error) (finishReason string, err error)

	// StreamChatEvents 与 StreamChat 相同，但把文本增量与拼接完成的工具调用
	// 都回调给 onEvent；工具调用事件保证了与文本增量的相对顺序。
	StreamChatEvents(ctx context.Context, req ChatRequest, onEvent func(ev StreamEvent) error) (finishReason string, err error)
}

// eventsToDelta 把事件回调退化为纯文本增量回调：StreamChat 实现的垫片。
func eventsToDelta(onDelta func(string) error) func(StreamEvent) error {
	return func(ev StreamEvent) error {
		if ev.Type == StreamEventTextDelta && ev.TextDelta != "" {
			return onDelta(ev.TextDelta)
		}
		return nil
	}
}

// httpClient 为两个实现共用：优先复用注入的 client，否则按 XxxMS 超时约定构建。
func httpClient(custom *http.Client, timeoutMS int) *http.Client {
	if custom != nil {
		return custom
	}
	if timeoutMS <= 0 {
		timeoutMS = DefaultTimeoutMS
	}
	return &http.Client{Timeout: time.Duration(timeoutMS) * time.Millisecond}
}

// httpError 统一包装非 2xx 的 LLM 响应，携带状态码与错误消息。
type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string {
	b := e.Body
	if len(b) > 200 {
		b = b[:200]
	}
	return fmt.Sprintf("LLM 服务返回 %d: %s", e.Status, b)
}

// checkResponseStatus 校验响应状态码：非 2xx 时读完响应体，
// 优先解析 {"error":{"message":...}}（OpenAI 与 Anthropic 的错误体结构一致），
// 解析失败则原样截断，以 httpError 返回。
func checkResponseStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	var body []byte
	if resp.Body != nil {
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
	}
	msg := strings.TrimSpace(string(body))
	var errBody struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errBody); err == nil && errBody.Error != nil && errBody.Error.Message != "" {
		msg = errBody.Error.Message
	}
	return &httpError{Status: resp.StatusCode, Body: msg}
}
