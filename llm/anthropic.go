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
		return nil, fmt.Errorf("创建 Anthropic 客户端需要先提供 apiKey")
	}
	if opts.Model == "" {
		return nil, fmt.Errorf("创建 Anthropic 客户端需要先提供 model")
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

// anthropicContentBlock 是 Messages content 块的统一线结构：
// text 块用 Type+Text；tool_use 块用 ID+Name+Input；
// tool_result 块用 ToolUseID+Text（结果正文）。
type anthropicContentBlock struct {
	Type      string          `json:"type"` // "text" | "tool_use" | "tool_result"
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use
	Name      string          `json:"name,omitempty"`        // tool_use
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use 的参数对象
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result 引用
}

// anthropicMessage 是 Messages 消息的线格式。Content 要么是纯字符串，
// 要么是块数组（assistant 带 tool_use、user 带 tool_result 时）。
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string 或 []anthropicContentBlock
}

// anthropicTool 是 tools 数组元素；ToolDef.Parameters 映射为 input_schema。
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicChatRequest 是 Messages API 请求体。
// system 走顶层字段（与 OpenAI 的 system 消息不同），max_tokens 必填。
type anthropicChatRequest struct {
	Model       string             `json:"model"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	MaxTokens   int                `json:"max_tokens"`
	Stream      bool               `json:"stream"`
	Temperature *float64           `json:"temperature,omitempty"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
}

// toAnthropicMessages 把厂商无关消息映射为线格式：
// assistant 带 ToolCalls 时 Content 变为块数组（正文非空则带 text 块）；
// 连续的 role "tool" 消息合批为一条 user 消息（Anthropic 要求同一轮的
// tool_result 全部收在同一 user turn 里，不接受连续 tool 角色）。
func toAnthropicMessages(msgs []ChatMessage) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			blocks := make([]anthropicContentBlock, 0, len(m.ToolCalls)+1)
			if m.Content != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicContentBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Name,
					Input: normalizeToolArgs(tc.Arguments),
				})
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
		case m.Role == "tool":
			// 连续 tool 结果合批进同一条 user 消息
			if len(out) > 0 {
				if last := &out[len(out)-1]; last.Role == "user" {
					if blocks, ok := last.Content.([]anthropicContentBlock); ok && consumingToolResults(blocks) {
						last.Content = append(blocks, anthropicContentBlock{
							Type: "tool_result", ToolUseID: m.ToolCallID, Text: m.Content,
						})
						continue
					}
				}
			}
			out = append(out, anthropicMessage{Role: "user", Content: []anthropicContentBlock{{
				Type: "tool_result", ToolUseID: m.ToolCallID, Text: m.Content,
			}}})
		default:
			out = append(out, anthropicMessage{Role: m.Role, Content: m.Content})
		}
	}
	return out
}

// consumingToolResults 判断块数组是否只含 tool_result（用于 tool 合批判断：
// 只有紧跟上一批 tool 结果的才并入，避免吞掉真正的用户文本）。
func consumingToolResults(blocks []anthropicContentBlock) bool {
	for _, b := range blocks {
		if b.Type != "tool_result" {
			return false
		}
	}
	return len(blocks) > 0
}

// anthropicEventData 是流式响应 data: 行的 JSON 结构，按事件类型取用不同字段。
type anthropicEventData struct {
	Type  string `json:"type"`
	Index int    `json:"index"` // content_block_* 事件携带块下标
	Delta struct {
		Type        string `json:"type"` // content_block_delta 时为 "text_delta" / "input_json_delta"
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"` // input_json_delta 的参数分片
	} `json:"delta"`
	ContentBlock struct {
		Type string `json:"type"` // content_block_start 的块类型："text" / "tool_use"
		ID   string `json:"id"`   // tool_use
		Name string `json:"name"` // tool_use
	} `json:"content_block"`
}

// anthropicBlockAccum 是按块下标累积中的 tool_use。
type anthropicBlockAccum struct {
	id   string
	name string
	args strings.Builder
}

// StreamChat 发起一次流式对话（POST /v1/messages, stream: true），只回调文本增量。
func (c *AnthropicClient) StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string) error) (string, error) {
	return c.StreamChatEvents(ctx, req, eventsToDelta(onDelta))
}

// StreamChatEvents 发起一次流式对话，文本增量与拼接完成的工具调用都经
// onEvent 回调。工具调用经 content_block_start(tool_use) +
// content_block_delta(input_json_delta) 累积，content_block_stop 时发出。
//
// 线协议事件集：message_start / content_block_start / content_block_delta /
// content_block_stop / message_delta / message_stop / ping。除上述关心的
// 事件外其余安全跳过。
func (c *AnthropicClient) StreamChatEvents(ctx context.Context, req ChatRequest, onEvent func(ev StreamEvent) error) (string, error) {
	model := req.Model
	if model == "" {
		model = c.model
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultAnthropicMaxTokens
	}

	wireReq := anthropicChatRequest{
		Model:       model,
		System:      req.SystemPrompt,
		Messages:    toAnthropicMessages(req.Messages),
		MaxTokens:   maxTokens,
		Stream:      true,
		Temperature: req.Temperature,
	}
	if len(req.Tools) > 0 {
		wireReq.Tools = make([]anthropicTool, 0, len(req.Tools))
		for _, td := range req.Tools {
			wireReq.Tools = append(wireReq.Tools, anthropicTool{
				Name:        td.Name,
				Description: td.Description,
				InputSchema: td.Parameters,
			})
		}
	}

	body, err := json.Marshal(wireReq)
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
	// 按块下标累积 tool_use；content_block_stop 时机发事件。
	blocks := map[int]*anthropicBlockAccum{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				blocks[event.Index] = &anthropicBlockAccum{
					id:   event.ContentBlock.ID,
					name: event.ContentBlock.Name,
				}
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				if event.Delta.Text != "" {
					if err := onEvent(StreamEvent{Type: StreamEventTextDelta, TextDelta: event.Delta.Text}); err != nil {
						return finishReason, err
					}
				}
			case "input_json_delta":
				if acc := blocks[event.Index]; acc != nil {
					acc.args.WriteString(event.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if acc := blocks[event.Index]; acc != nil {
				delete(blocks, event.Index)
				if acc.name != "" {
					if err := onEvent(StreamEvent{Type: StreamEventToolCall, ToolCall: ToolCall{
						ID:        acc.id,
						Name:      acc.name,
						Arguments: normalizeToolArgs([]byte(acc.args.String())),
					}}); err != nil {
						return finishReason, err
					}
				}
			}
		case "message_delta":
			// stop_reason 在 delta.stop_reason（data 再解一层）。
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
	// 服务端未发 message_stop 直接关流：视为正常结束（未关闭的累积块丢弃）。
	return finishReason, nil
}
