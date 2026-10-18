package llm

import (
	"context"
	"encoding/json"
	"io"
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

// openAIToolSSE 构造带 tool_calls 分片的 SSE 流：先文本增量，再两个
// 工具调用的分片（首个分片带 id/name，后续只有 arguments 片段）。
func openAIToolSSE(t *testing.T) string {
	t.Helper()
	return strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"生成中…"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"send_template_card","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"card\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"card_type\":\"text_notice\"}}"}}]}}]}`,
		// 第二个调用：无参数 → 归一 {}
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"send_markdown","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
}

func TestOpenAIToolCallStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(openAIToolSSE(t)))
	}))
	t.Cleanup(srv.Close)

	client, err := NewOpenAIClient(OpenAIOptions{APIKey: "k", BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	var events []StreamEvent
	finishReason, err := client.StreamChatEvents(context.Background(), ChatRequest{}, func(ev StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChatEvents 失败: %v", err)
	}
	if finishReason != "tool_calls" {
		t.Errorf("finishReason = %q, 期望 tool_calls", finishReason)
	}
	// 事件序：文本增量在前，两个工具调用按 index 序在后
	if len(events) != 3 {
		t.Fatalf("应有 1 文本 + 2 工具事件, 实际 %d", len(events))
	}
	if events[0].Type != StreamEventTextDelta || events[0].TextDelta != "生成中…" {
		t.Errorf("首个事件应为文本增量: %+v", events[0])
	}
	tc1, tc2 := events[1].ToolCall, events[2].ToolCall
	if events[1].Type != StreamEventToolCall || tc1.ID != "call_1" || tc1.Name != "send_template_card" {
		t.Errorf("第一个工具调用不对: %+v", events[1])
	}
	if string(tc1.Arguments) != `{"card":{"card_type":"text_notice"}}` {
		t.Errorf("分片拼接参数不对: %s", tc1.Arguments)
	}
	if tc2.ID != "call_2" || string(tc2.Arguments) != "{}" {
		t.Errorf("空参数应归一 {}: %+v", tc2)
	}
}

func TestOpenAIRequestToolWire(t *testing.T) {
	// 请求方向：小写键、tools+tool_choice、assistant tool_calls（arguments
	// 为 JSON 字符串）、role "tool" 带 tool_call_id；Tools 空时不发 tools。
	var rawReq []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawReq, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(openAISSE(t, []string{"ok"}, "stop")))
	}))
	t.Cleanup(srv.Close)

	client, err := NewOpenAIClient(OpenAIOptions{APIKey: "k", BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	req := ChatRequest{
		SystemPrompt: "你是助手",
		Messages: []ChatMessage{
			{Role: "user", Content: "发卡片"},
			{Role: "assistant", Content: "好的", ToolCalls: []ToolCall{{
				ID: "call_1", Name: "send_template_card",
				Arguments: json.RawMessage(`{"card":{"card_type":"text_notice"}}`),
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: `{"ok":true}`},
		},
		Tools: []ToolDef{{Name: "send_template_card", Description: "发卡片",
			Parameters: map[string]any{"type": "object"}}},
	}
	if _, err := client.StreamChat(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}

	var parsed struct {
		Messages []struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice string `json:"tool_choice"`
	}
	if err := json.Unmarshal(rawReq, &parsed); err != nil {
		t.Fatalf("解析请求失败: %v", err)
	}
	// SystemPrompt 以首条 system 消息注入（小写 role/content 键）
	if len(parsed.Messages) != 4 || parsed.Messages[0].Role != "system" || parsed.Messages[0].Content != "你是助手" {
		t.Errorf("system 注入或键大小写不对: %+v", parsed.Messages[0])
	}
	asst := parsed.Messages[2]
	if asst.Role != "assistant" || len(asst.ToolCalls) != 1 {
		t.Fatalf("assistant 应带 tool_calls: %+v", asst)
	}
	if asst.ToolCalls[0].ID != "call_1" || asst.ToolCalls[0].Function.Name != "send_template_card" {
		t.Errorf("tool_calls 不对: %+v", asst.ToolCalls[0])
	}
	if asst.ToolCalls[0].Function.Arguments != `{"card":{"card_type":"text_notice"}}` {
		t.Errorf("arguments 应为 JSON 字符串: %s", asst.ToolCalls[0].Function.Arguments)
	}
	toolMsg := parsed.Messages[3]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Content != `{"ok":true}` {
		t.Errorf("role tool 消息不对: %+v", toolMsg)
	}
	if len(parsed.Tools) != 1 || parsed.Tools[0].Type != "function" || parsed.Tools[0].Function.Name != "send_template_card" {
		t.Errorf("tools 目录不对: %+v", parsed.Tools)
	}
	if parsed.ToolChoice != "auto" {
		t.Errorf("tools 非空时 tool_choice 应为 auto: %q", parsed.ToolChoice)
	}

	// Tools 为空时不发 tools/tool_choice 字段
	rawReq = nil
	if _, err := client.StreamChat(context.Background(), ChatRequest{}, func(string) error { return nil }); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}
	if strings.Contains(string(rawReq), `"tools"`) || strings.Contains(string(rawReq), `"tool_choice"`) {
		t.Errorf("Tools 空时不应发 tools/tool_choice: %s", rawReq)
	}
}

// TestOpenAIStreamErrorChunk 流中途的 OpenAI 兼容网关 error 块
// （data: {"error":{...}}）应以错误返回——它不带 choices，旧实现反序列化
// 「成功」得到零 Choices，错误被静默吞掉、伪装成空回复。
func TestOpenAIStreamErrorChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + `{"choices":[{"delta":{"content":"半截"}}]}` + "\n\n" +
			"data: " + `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}` + "\n\n" +
			"data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	client, err := NewOpenAIClient(OpenAIOptions{
		APIKey: "test-key", BaseURL: srv.URL, Model: "gpt-x",
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	var deltas []string
	_, err = client.StreamChat(context.Background(), ChatRequest{
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	}, func(delta string) error {
		deltas = append(deltas, delta)
		return nil
	})
	if err == nil {
		t.Fatal("error 块应导致 StreamChat 返回错误, 不能伪装成空回复")
	}
	if !strings.Contains(err.Error(), "Rate limit exceeded") {
		t.Errorf("错误应携带服务端信息: %v", err)
	}
	if len(deltas) != 1 || deltas[0] != "半截" {
		t.Errorf("error 块前的增量应已送达: %v", deltas)
	}
}
