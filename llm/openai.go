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

// openAIChatRequest 是 chat/completions 请求体。
type openAIChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
}

// openAIStreamChunk 是流式响应里一个 data: 行的 JSON 结构。
type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			// Content 是正文增量。
			Content string `json:"content"`
			// ReasoningContent 是推理模型的思维链增量（DeepSeek-R1 / GLM 等），一并透传。
			ReasoningContent *string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// StreamChat 发起一次流式对话（chat/completions, stream: true）。
func (c *OpenAIClient) StreamChat(ctx context.Context, req ChatRequest, onDelta func(delta string) error) (string, error) {
	model := req.Model
	if model == "" {
		model = c.model
	}
	// 每次调用克隆 messages：SystemPrompt 以 system 消息注入头部。
	messages := make([]ChatMessage, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: req.SystemPrompt})
	}
	messages = append(messages, req.Messages...)

	body, err := json.Marshal(openAIChatRequest{
		Model:       model,
		Messages:    messages,
		Stream:      true,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	})
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
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		data := trimSSEData(scanner.Text())
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return finishReason, nil
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return finishReason, fmt.Errorf("解析 OpenAI 流式响应失败: %w", err)
		}
		for _, choice := range chunk.Choices {
			// 推理模型（GLM/DeepSeek-R1）的思维链增量（reasoning_content）默认
			// 只在 IncludeReasoning 开启时透传；消息机器人场景通常只发正文。
			if c.includeReasoning && choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
				if err := onDelta(*choice.Delta.ReasoningContent); err != nil {
					return finishReason, err
				}
			}
			if choice.Delta.Content != "" {
				if err := onDelta(choice.Delta.Content); err != nil {
					return finishReason, err
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finishReason = *choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return finishReason, err
	}
	// 服务端未发 [DONE] 直接关流：视为正常结束。
	return finishReason, nil
}
