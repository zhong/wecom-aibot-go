// Package llm 提供与企业微信智能机器人 SDK 配套的大模型对接能力：
// 统一的流式对话抽象 Client、OpenAI 兼容与 Anthropic Claude 两种实现，
// 以及把 LLM 增量桥接到企微流式回复帧的 StreamReply。
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

// DefaultTimeout 是 LLM 流式请求的整体默认超时（毫秒），
// 命名与根包 Config 的 XxxMS / DefaultXxx 约定保持一致。
const DefaultTimeoutMS = 120000

// ChatMessage 是一条对话消息，Role 取 "system" / "user" / "assistant"。
type ChatMessage struct {
	Role    string
	Content string
}

// ChatRequest 是一次与厂商无关的流式对话请求。
type ChatRequest struct {
	Model        string // 必填
	Messages     []ChatMessage
	SystemPrompt string   // 非空时作为 system 注入：OpenAI 拼到 messages 头部，Anthropic 用顶层 system 字段
	Temperature  *float64 // nil 表示用服务端默认
	MaxTokens    int      // 0 表示用服务端默认；Anthropic 端点 max_tokens 必填，客户端兜底 1024
}

// Client 是流式对话客户端的统一抽象：每个内容增量回调一次 onDelta，
// 内容由调用方累积。onDelta 返回 error 可提前中止流式读取并向上传播。
type Client interface {
	// StreamChat 发起一次流式对话，返回结束原因（如 "stop" / "length"，供观测）
	// 或错误。ctx 取消会中断底层 HTTP 请求。
	StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string) error) (finishReason string, err error)
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
