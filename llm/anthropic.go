package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// DefaultAnthropicBaseURL 是 Anthropic Messages API 的默认地址。
const DefaultAnthropicBaseURL = "https://api.anthropic.com"

// DefaultAnthropicMaxTokens 是 Anthropic 端点 max_tokens 必填时的客户端兜底值。
const DefaultAnthropicMaxTokens = 1024

// AnthropicClient 对接 Anthropic Claude Messages 流式端点。
type AnthropicClient struct {
	apiKey  string
	baseURL string
	model   string
	http    *http.Client
}

// AnthropicOptions 是 AnthropicClient 的配置。
type AnthropicOptions struct {
	APIKey     string       // 必填
	BaseURL    string       // 默认 "https://api.anthropic.com"；网关/代理改这里
	Model      string       // 必填，如 "claude-sonnet-5-5"
	TimeoutMS  int          // 默认 DefaultTimeoutMS
	HTTPClient *http.Client // 可选注入（测试用）
}

// NewAnthropicClient 创建 Anthropic 客户端，校验必填项。
func NewAnthropicClient(opts AnthropicOptions) (*AnthropicClient, error) {
	if opts.APIKey == "" {
		return nil, fmt.Errorf("Anthropic 客户端缺少 APIKey")
	}
	if opts.Model == "" {
		return nil, fmt.Errorf("Anthropic 客户端缺少 Model")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultAnthropicBaseURL
	}
	return &AnthropicClient{
		apiKey:  opts.APIKey,
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		model:   opts.Model,
		http:    httpClient(opts.HTTPClient, opts.TimeoutMS),
	}, nil
}

// anthropicChatRequest 是 Messages API 请求体。
// system 走顶层字段（与 OpenAI 的 system 消息不同），max_tokens 必填。
type anthropicChatRequest struct {
	Model       string        `json:"model"`
	System      string        `json:"system,omitempty"`
	Messages    []ChatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
}

// anthropicEventData 是流式响应 data: 行的 JSON 结构，按事件类型取用不同字段。
type anthropicEventData struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"` // content_block_delta 时为 "text_delta" / "thinking_delta"
		Text string `json:"text"`
	} `json:"delta"`
}

// StreamChat 发起一次流式对话（POST /v1/messages, stream: true）。
//
// 线协议事件集：message_start / content_block_start / content_block_delta /
// content_block_stop / message_delta / message_stop / ping。只关心
// content_block_delta 的 text_delta 与 message_stop，其余事件安全跳过。
func (c *AnthropicClient) StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string) error) (string, error) {
	model := req.Model
	if model == "" {
		model = c.model
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultAnthropicMaxTokens
	}

	body, err := json.Marshal(anthropicChatRequest{
		Model:       model,
		System:      req.SystemPrompt,
		Messages:    req.Messages,
		MaxTokens:   maxTokens,
		Stream:      true,
		Temperature: req.Temperature,
	})
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	if err := checkResponseStatus(resp); err != nil {
		return "", err
	}
	defer resp.Body.Close()

	finishReason := ""
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		data := trimSSEData(scanner.Text())
		if data == "" {
			continue
		}
		var event anthropicEventData
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return finishReason, fmt.Errorf("解析 Anthropic 流式响应失败: %w", err)
		}
		switch event.Type {
		case "content_block_delta":
			if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
				if err := onDelta(event.Delta.Text); err != nil {
					return finishReason, err
				}
			}
		case "message_delta":
			// 这个事件里没有 text_delta 的 text。
			// stop_reason 另有字段（delta.stop_reason），简化处理：由 data 再解一层。
			var md struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &md); err == nil && md.Delta.StopReason != "" {
				finishReason = md.Delta.StopReason
			}
		case "message_stop":
			return finishReason, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return finishReason, err
	}
	// 服务端未发 message_stop 直接关流：视为正常结束。
	return finishReason, nil
}
