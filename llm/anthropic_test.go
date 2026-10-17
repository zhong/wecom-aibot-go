package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errorsIs / errorsAntiCircular 是 errors.Is / errors.As 的直通别名，
// 供测试文件统一引用（避免每个测试文件重复 import errors 却只用一次）。
var (
	errAbort = errors.New("abort")
)

func errorsIs(err, target error) bool { return errors.Is(err, target) }

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// anthropicSSE 把文本增拼成 Anthropic 风格的 SSE 事件流：
// message_start / content_block_delta(text_delta) / message_delta(stop_reason) / message_stop。
func anthropicSSE(t *testing.T, texts []string, stopReason string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1"}}` + "\n\n")
	for _, s := range texts {
		b.WriteString(`event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":` + jsonString(t, s) + `}}` + "\n\n")
	}
	if stopReason != "" {
		b.WriteString(`event: message_delta` + "\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":` + jsonString(t, stopReason) + `}}` + "\n\n")
	}
	b.WriteString(`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n")
	return b.String()
}

func TestAnthropicClientStreamChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			t.Errorf("请求路径不对: %s", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key 头不对: %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Errorf("anthropic-version 头不对: %q", got)
		}
		var body anthropicChatRequest
		if err := jsonNewDecoder(r).Decode(&body); err != nil {
			t.Errorf("解析请求体失败: %v", err)
		}
		if !body.Stream {
			t.Error("stream 必须为 true")
		}
		if body.MaxTokens <= 0 {
			t.Error("Anthropic 端 max_tokens 必填, 客户端应兜底")
		}
		if body.System != "你是助手" {
			t.Errorf("SystemPrompt 应走顶层 system 字段: %q", body.System)
		}
		for _, m := range body.Messages {
			if m.Role == "system" {
				t.Error("Anthropic 不应把 system 混进 messages")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicSSE(t, []string{"你好", "，", "世界"}, "end_turn")))
	}))
	t.Cleanup(srv.Close)

	client, err := NewAnthropicClient(AnthropicOptions{
		APIKey:  "test-key",
		BaseURL: srv.URL,
		Model:   "claude-sonnet-5-5",
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}

	var got []string
	finishReason, err := client.StreamChat(context.Background(), ChatRequest{
		Messages:     []ChatMessage{{Role: "user", Content: "打个招呼"}},
		SystemPrompt: "你是助手",
	}, func(delta string) error {
		got = append(got, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if finishReason != "end_turn" {
		t.Errorf("finishReason = %q, 期望 end_turn", finishReason)
	}
	if strings.Join(got, "") != "你好，世界" {
		t.Errorf("增量拼接结果不对: %q", strings.Join(got, ""))
	}
}

func TestAnthropicClientMaxTokensFallback(t *testing.T) {
	// 请求未设 MaxTokens 时客户端必须兜底非零值（端点必填）。
	var got int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body anthropicChatRequest
		if err := jsonNewDecoder(r).Decode(&body); err == nil {
			got = body.MaxTokens
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicSSE(t, []string{"ok"}, "")))
	}))
	t.Cleanup(srv.Close)

	client, err := NewAnthropicClient(AnthropicOptions{APIKey: "k", BaseURL: srv.URL, Model: "claude-sonnet-5-5"})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	if _, err := client.StreamChat(context.Background(), ChatRequest{}, func(string) error { return nil }); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if got != DefaultAnthropicMaxTokens {
		t.Errorf("MaxTokens 兜底 = %d, 期望 %d", got, DefaultAnthropicMaxTokens)
	}
}

func TestAnthropicClientHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"max_tokens is required"}}`))
	}))
	t.Cleanup(srv.Close)

	client, err := NewAnthropicClient(AnthropicOptions{APIKey: "k", BaseURL: srv.URL, Model: "claude-sonnet-5-5"})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	_, err = client.StreamChat(context.Background(), ChatRequest{}, func(string) error { return nil })
	var httpErr *httpError
	if !errorsAs(err, &httpErr) {
		t.Fatalf("期望 httpError, 实际 %T", err)
	}
	if httpErr.Status != http.StatusBadRequest {
		t.Errorf("状态码 = %d, 期望 400", httpErr.Status)
	}
}

func TestAnthropicClientSkipNonTextEvents(t *testing.T) {
	// ping / content_block_start / thinking_delta 等事件应安全跳过，不产生增量。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		body := strings.Join([]string{
			`event: ping` + "\n" + `data: {"type":"ping"}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"..."}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"正文"}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		}, "\n\n") + "\n\n"
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	client, err := NewAnthropicClient(AnthropicOptions{APIKey: "k", BaseURL: srv.URL, Model: "claude-sonnet-5-5"})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	var got []string
	if _, err := client.StreamChat(context.Background(), ChatRequest{}, func(delta string) error {
		got = append(got, delta)
		return nil
	}); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if len(got) != 1 || got[0] != "正文" {
		t.Errorf("只应透传 text_delta: %q", got)
	}
}

func TestNewClientsValidation(t *testing.T) {
	if _, err := NewOpenAIClient(OpenAIOptions{Model: "m"}); err == nil {
		t.Error("OpenAI 缺 APIKey 应报错")
	}
	if _, err := NewOpenAIClient(OpenAIOptions{APIKey: "k"}); err == nil {
		t.Error("OpenAI 缺 Model 应报错")
	}
	if _, err := NewAnthropicClient(AnthropicOptions{Model: "m"}); err == nil {
		t.Error("Anthropic 缺 APIKey 应报错")
	}
	if _, err := NewAnthropicClient(AnthropicOptions{APIKey: "k"}); err == nil {
		t.Error("Anthropic 缺 Model 应报错")
	}
}
