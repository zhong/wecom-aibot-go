package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jsonString 用 json.Marshal 生成合法 JSON 字符串字面量，避免手写转义。
func jsonString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	return string(b)
}

// openAISSE 把若干 content 增量拼成 OpenAI 风格的 SSE 响应体，末尾带
// finish_reason 与 [DONE]。
func openAISSE(t *testing.T, deltas []string, finishReason string) string {
	t.Helper()
	var b strings.Builder
	for _, d := range deltas {
		b.WriteString(`data: {"choices":[{"delta":{"content":` + jsonString(t, d) + `}}]}` + "\n\n")
	}
	if finishReason != "" {
		b.WriteString(`data: {"choices":[{"delta":{},"finish_reason":` + jsonString(t, finishReason) + `}]}` + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestOpenAIClientStreamChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("请求路径不对: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization 头不对: %q", got)
		}
		var body openAIChatRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析请求体失败: %v", err)
		}
		if !body.Stream {
			t.Error("stream 必须为 true")
		}
		if len(body.Messages) == 0 || body.Messages[0].Role != "system" || body.Messages[0].Content != "你是助手" {
			t.Errorf("SystemPrompt 未注入头部: %+v", body.Messages)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(openAISSE(t, []string{"你好", "，", "世界"}, "stop")))
	}))
	t.Cleanup(srv.Close)

	client, err := NewOpenAIClient(OpenAIOptions{
		APIKey:  "test-key",
		BaseURL: srv.URL,
		Model:   "gpt-4o",
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
	if finishReason != "stop" {
		t.Errorf("finishReason = %q, 期望 stop", finishReason)
	}
	if strings.Join(got, "") != "你好，世界" {
		t.Errorf("增量拼接结果不对: %q", strings.Join(got, ""))
	}
}

func TestOpenAIClientReasoningContent(t *testing.T) {
	// GLM/DeepSeek-R1 网关把思维链放 reasoning_content，与正文增量交错下发。
	streamBody := `data: {"choices":[{"delta":{"reasoning_content":"思考中"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"答案"}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	// 默认不透传 CoT，只发正文
	t.Run("默认只透传正文", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(streamBody))
		}))
		t.Cleanup(srv.Close)
		client, err := NewOpenAIClient(OpenAIOptions{APIKey: "k", BaseURL: srv.URL, Model: "glm-4"})
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
		if len(got) != 1 || got[0] != "答案" {
			t.Errorf("默认应只透传正文: %q", got)
		}
	})

	// IncludeReasoning 开启时 CoT 与正文一并透传
	t.Run("IncludeReasoning透传思维链", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(streamBody))
		}))
		t.Cleanup(srv.Close)
		client, err := NewOpenAIClient(OpenAIOptions{APIKey: "k", BaseURL: srv.URL, Model: "glm-4", IncludeReasoning: true})
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
		if len(got) != 2 || got[0] != "思考中" || got[1] != "答案" {
			t.Errorf("增量序列不对: %q", got)
		}
	})
}

func TestOpenAIClientHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	t.Cleanup(srv.Close)

	client, err := NewOpenAIClient(OpenAIOptions{APIKey: "bad", BaseURL: srv.URL, Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	_, err = client.StreamChat(context.Background(), ChatRequest{}, func(string) error { return nil })
	if err == nil {
		t.Fatal("期望返回错误")
	}
	var httpErr *httpError
	if !errorsAs(err, &httpErr) {
		t.Fatalf("期望 httpError, 实际 %T", err)
	}
	if httpErr.Status != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, 期望 401", httpErr.Status)
	}
	if httpErr.Body != "invalid api key" {
		t.Errorf("错误消息未按 error.message 提取: %q", httpErr.Body)
	}
}

func TestOpenAIClientOnDeltaAbort(t *testing.T) {
	// onDelta 返回 error 时应中止读取并传播错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(openAISSE(t, []string{"a", "b", "c"}, "")))
	}))
	t.Cleanup(srv.Close)

	client, err := NewOpenAIClient(OpenAIOptions{APIKey: "k", BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	calls := 0
	_, err = client.StreamChat(context.Background(), ChatRequest{}, func(string) error {
		calls++
		if calls == 2 {
			return errAbort
		}
		return nil
	})
	if !errorsIs(err, errAbort) {
		t.Errorf("期望 onDelta 的错误向上传播, 实际: %v", err)
	}
	if calls != 2 {
		t.Errorf("应停在第二个增量, 实际调用了 %d 次", calls)
	}
}

func TestOpenAIClientCtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	client, err := NewOpenAIClient(OpenAIOptions{APIKey: "k", BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.StreamChat(ctx, ChatRequest{}, func(string) error { return nil })
	if err == nil {
		t.Fatal("ctx 取消后期望错误")
	}
}
