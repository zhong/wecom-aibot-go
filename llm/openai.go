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

// DefaultOpenAIBaseURL 是 OpenAI 兼容端点的默认地址，
// DeepSeek/GLM/Qwen/vLLM/one-api 等网关通过 BaseURL 覆盖。
const DefaultOpenAIBaseURL = "https://api.openai.com/v1"

// OpenAIClient 对接 OpenAI 兼容 chat-completions 流式端点。
type OpenAIClient struct {
	apiKey           string
	baseURL          string
	model            string
	includeReasoning bool
	http             *http.Client
}

// OpenAIOptions 是 OpenAIClient 的配置。
type OpenAIOptions struct {
	APIKey     string       // 必填
	BaseURL    string       // 默认 "https://api.openai.com/v1"；自建/网关改这里
	Model      string       // 必填，如 "gpt-4o"、"deepseek-chat"
	TimeoutMS  int          // 默认 DefaultTimeoutMS
	HTTPClient *http.Client // 可选注入（测试用）
	// IncludeReasoning 控制是否透传推理模型（GLM/DeepSeek-R1 等）的思维链增量
	// （delta.reasoning_content）。默认 false 只发正文，适合消息机器人场景；
	// 设为 true 时 CoT 与正文增量一并回调，由调用方决定展示策略。
	IncludeReasoning bool
}

// NewOpenAIClient 创建 OpenAI 兼容客户端，校验必填项。
func NewOpenAIClient(opts OpenAIOptions) (*OpenAIClient, error) {
	if opts.APIKey == "" {
		return nil, fmt.Errorf("OpenAI 客户端缺少 APIKey")
	}
	if opts.Model == "" {
		return nil, fmt.Errorf("OpenAI 客户端缺少 Model")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultOpenAIBaseURL
	}
	return &OpenAIClient{
		apiKey:           opts.APIKey,
		baseURL:          strings.TrimRight(opts.BaseURL, "/"),
		model:            opts.Model,
		includeReasoning: opts.IncludeReasoning,
		http:             httpClient(opts.HTTPClient, opts.TimeoutMS),
	}, nil
}

// openAIMessage 是 chat/completions 消息的线格式（小写键）。
// tool_calls 仅 assistant 携带；tool_call_id 仅 role "tool" 携带。
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"` // 纯 tool_calls 的 assistant 可为空串
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openAIToolCall 是线上的工具调用结构；function.arguments 是 JSON 字符串。
type openAIToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // 固定 "function"
	Function openAIToolCallFn `json:"function"`
}

type openAIToolCallFn struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON 字符串（请求方向）
}

// openAITool 是 tools 数组元素；function 直接复用厂商无关的 ToolDef。
type openAITool struct {
	Type     string  `json:"type"` // 固定 "function"`
	Function ToolDef `json:"function"`
}

// openAIChatRequest 是 chat/completions 请求体。
type openAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Tools       []openAITool    `json:"tools,omitempty"`
	ToolChoice  string          `json:"tool_choice,omitempty"` // tools 非空时为 "auto"
}

// toOpenAIMessages 把厂商无关消息映射为线格式：SystemPrompt 以 system
// 注入头部，assistant 的 ToolCalls 展开为 tool_calls，role "tool" 原样映射。
func toOpenAIMessages(systemPrompt string, msgs []ChatMessage) []openAIMessage {
	out := make([]openAIMessage, 0, len(msgs)+1)
	if systemPrompt != "" {
		out = append(out, openAIMessage{Role: "system", Content: systemPrompt})
	}
	for _, m := range msgs {
		om := openAIMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		if len(m.ToolCalls) > 0 {
			om.ToolCalls = make([]openAIToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				om.ToolCalls = append(om.ToolCalls, openAIToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openAIToolCallFn{
						Name:      tc.Name,
						Arguments: string(tc.Arguments),
					},
				})
			}
		}
		out = append(out, om)
	}
	return out
}

// openAIStreamChunk 是流式响应里一个 data: 行的 JSON 结构。
type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			// Content 是正文增量。
			Content string `json:"content"`
			// ReasoningContent 是推理模型的思维链增量（DeepSeek-R1 / GLM 等）。
			ReasoningContent *string `json:"reasoning_content"`
			// ToolCalls 是工具调用分片：首个分片带 index/id/name，
			// 后续分片只带 index 与 arguments 的 JSON 片段，逐片拼接。
			ToolCalls []openAIStreamToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// openAIStreamToolCall 是流式 delta.tool_calls 的分片结构。
type openAIStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"` // 仅首个分片携带
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`      // 仅首个分片携带
		Arguments string `json:"arguments"` // JSON 分片，逐片拼接
	} `json:"function"`
}

// toolCallAccum 是按 index 累积中的工具调用。
type toolCallAccum struct {
	id   string
	name string
	args strings.Builder
}

// StreamChat 发起一次流式对话（chat/completions, stream: true），只回调文本增量。
func (c *OpenAIClient) StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string) error) (string, error) {
	return c.StreamChatEvents(ctx, req, eventsToDelta(onDelta))
}

// StreamChatEvents 发起一次流式对话，文本增量与拼接完成的工具调用
// 都经 onEvent 回调。工具调用分片按 delta.tool_calls[].index 累积，
// [DONE]/EOF 时按 index 序一次性发出（保证与文本增量的相对顺序）。
func (c *OpenAIClient) StreamChatEvents(ctx context.Context, req ChatRequest, onEvent func(ev StreamEvent) error) (string, error) {
	model := req.Model
	if model == "" {
		model = c.model
	}

	wireReq := openAIChatRequest{
		Model:       model,
		Messages:    toOpenAIMessages(req.SystemPrompt, req.Messages),
		Stream:      true,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}
	if len(req.Tools) > 0 {
		wireReq.Tools = make([]openAITool, 0, len(req.Tools))
		for _, td := range req.Tools {
			wireReq.Tools = append(wireReq.Tools, openAITool{Type: "function", Function: td})
		}
		wireReq.ToolChoice = "auto"
	}

	body, err := json.Marshal(wireReq)
	if err != nil {
		return "", err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
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
	// 按 index 累积工具调用分片；最终按序发 StreamEventToolCall。
	var toolAccs []*toolCallAccum
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		data := trimSSEData(scanner.Text())
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return finishReason, fmt.Errorf("解析 OpenAI 流式响应失败: %w", err)
		}
		for _, choice := range chunk.Choices {
			// 推理模型（GLM/DeepSeek）的思维链增量（reasoning_content）默认
			// 只在 IncludeReasoning 开启时透传；消息机器人场景通常只发正文。
			if c.includeReasoning && choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
				if err := onEvent(StreamEvent{Type: StreamEventTextDelta, TextDelta: *choice.Delta.ReasoningContent}); err != nil {
					return finishReason, err
				}
			}
			if choice.Delta.Content != "" {
				if err := onEvent(StreamEvent{Type: StreamEventTextDelta, TextDelta: choice.Delta.Content}); err != nil {
					return finishReason, err
				}
			}
			for _, tc := range choice.Delta.ToolCalls {
				for tc.Index >= len(toolAccs) {
					toolAccs = append(toolAccs, &toolCallAccum{})
				}
				acc := toolAccs[tc.Index]
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				acc.args.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return finishReason, err
	}
	// [DONE] 或流自然关闭后：按 index 顺序发出拼接完成的工具调用。
	for _, acc := range toolAccs {
		if acc.name == "" {
			continue
		}
		if err := onEvent(StreamEvent{Type: StreamEventToolCall, ToolCall: ToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: normalizeToolArgs([]byte(acc.args.String())),
		}}); err != nil {
			return finishReason, err
		}
	}
	return finishReason, nil
}
