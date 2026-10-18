package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodedBlock 是 tool 请求断言用的 content 块解码结构。
type decodedBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

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
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
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
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
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

func TestAnthropicToolUseStream(t *testing.T) {
	// tool_use 经 content_block_start/delta(input_json_delta)/stop 累积装配，
	// stop_reason "tool_use" 流经 finishReason。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		body := strings.Join([]string{
			`event: message_start` + "\n" + `data: {"type":"message_start"}`,
			// 前置文本 + tool_use 块开始
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"生成中…"}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"send_template_card"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"card\":"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"card_type\":\"text_notice\"}}"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}`,
			// 空参数 tool_use：归一为 {}
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_2","name":"send_markdown"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":2}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		}, "\n\n") + "\n\n"
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	client, err := NewAnthropicClient(AnthropicOptions{APIKey: "k", BaseURL: srv.URL, Model: "claude-sonnet-5-5"})
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
	if finishReason != "tool_use" {
		t.Errorf("finishReason = %q, 期望 tool_use", finishReason)
	}

	var texts, tools []ToolCall
	for _, ev := range events {
		switch ev.Type {
		case StreamEventTextDelta:
			texts = append(texts, ToolCall{ID: ev.TextDelta})
		case StreamEventToolCall:
			tools = append(tools, ev.ToolCall)
		}
	}
	if len(texts) != 1 || texts[0].ID != "生成中…" {
		t.Errorf("文本增量不对: %+v", texts)
	}
	if len(tools) != 2 {
		t.Fatalf("应装配两个工具调用, 实际 %d", len(tools))
	}
	if tools[0].ID != "toolu_1" || tools[0].Name != "send_template_card" {
		t.Errorf("第一个工具调用不对: %+v", tools[0])
	}
	if string(tools[0].Arguments) != `{"card":{"card_type":"text_notice"}}` {
		t.Errorf("分片拼接参数不对: %s", tools[0].Arguments)
	}
	if string(tools[1].Arguments) != "{}" {
		t.Errorf("空参数应归一 {}: %s", tools[1].Arguments)
	}
}

func TestAnthropicRequestToolMapping(t *testing.T) {
	// 请求方向：assistant ToolCalls → tool_use 块；连续 role "tool"
	// 消息合批为一条 user 多 tool_result。用 json.RawMessage 保存原始
	// 请求捕获后断言（避免二次解码丢 any）。
	var rawReq []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawReq, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicSSE(t, []string{"ok"}, "")))
	}))
	t.Cleanup(srv.Close)

	client, err := NewAnthropicClient(AnthropicOptions{APIKey: "k", BaseURL: srv.URL, Model: "claude"})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	req := ChatRequest{
		SystemPrompt: "你是助手",
		Messages: []ChatMessage{
			{Role: "user", Content: "发卡片"},
			{Role: "assistant", Content: "好的", ToolCalls: []ToolCall{{
				ID: "toolu_1", Name: "send_template_card",
				Arguments: json.RawMessage(`{"card":{"card_type":"text_notice"}}`),
			}}},
			{Role: "tool", ToolCallID: "toolu_1", Content: `{"ok":true}`},
			{Role: "tool", ToolCallID: "toolu_2", Content: `{"ok":true}`},
			{Role: "user", Content: "继续"},
		},
		Tools: []ToolDef{{
			Name: "send_template_card", Description: "发卡片",
			Parameters: map[string]any{"type": "object"},
		}},
	}
	if _, err := client.StreamChat(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatalf("StreamChat 失败: %v", err)
	}

	var parsed struct {
		System   string                `json:"system"`
		Messages []decodedAnthropicMsg `json:"messages"`
		Tools    []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(rawReq, &parsed); err != nil {
		t.Fatalf("解析请求失败: %v", err)
	}
	if parsed.System != "你是助手" {
		t.Errorf("system 应在顶层: %q", parsed.System)
	}
	if len(parsed.Messages) != 4 {
		t.Fatalf("消息数不对（连续 tool 应合批）: %+v", parsed.Messages)
	}
	// assistant 块：text + tool_use
	asst := parsed.Messages[1]
	if asst.Role != "assistant" || len(asst.Content) != 2 {
		t.Fatalf("assistant 应为 [text, tool_use]: %+v", asst)
	}
	if asst.Content[0].Type != "text" || asst.Content[0].Text != "好的" {
		t.Errorf("text 块不对: %+v", asst.Content[0])
	}
	tu := asst.Content[1]
	if tu.Type != "tool_use" || tu.ID != "toolu_1" || tu.Name != "send_template_card" {
		t.Errorf("tool_use 块不对: %+v", tu)
	}
	if string(tu.Input) != `{"card":{"card_type":"text_notice"}}` {
		t.Errorf("tool_use input 不对: %s", tu.Input)
	}
	// 两个连续 tool 结果合批进一条 user
	merged := parsed.Messages[2]
	if merged.Role != "user" || len(merged.Content) != 2 {
		t.Fatalf("连续 tool 应合批为一条 user: %+v", merged)
	}
	for i, tr := range merged.Content {
		if tr.Type != "tool_result" {
			t.Errorf("块 %d 应为 tool_result: %+v", i, tr)
		}
	}
	if merged.Content[0].ToolUseID != "toolu_1" || merged.Content[1].ToolUseID != "toolu_2" {
		t.Errorf("tool_result 引用不对: %+v", merged.Content)
	}
	// 真正的用户消息不并入合批（content 为纯字符串线格式）
	if parsed.Messages[3].Role != "user" || parsed.Messages[3].ContentPlain != "继续" {
		t.Errorf("后续用户消息不应被合批吞掉: %+v", parsed.Messages[3])
	}
	if len(parsed.Tools) != 1 || parsed.Tools[0].Name != "send_template_card" {
		t.Errorf("tools 目录不对: %+v", parsed.Tools)
	}
	if parsed.Tools[0].InputSchema["type"] != "object" {
		t.Errorf("input_schema 应映射 Parameters: %+v", parsed.Tools[0].InputSchema)
	}
}

// decodedAnthropicMsg 是请求断言用的宽松解码结构：Content 兼容纯字符串
// （普通 user）与块数组（tool_use / tool_result）两种线格式。
type decodedAnthropicMsg struct {
	Role         string         `json:"role"`
	Content      []decodedBlock `json:"content"`
	ContentPlain string         `json:"-"`
}

func (m *decodedAnthropicMsg) UnmarshalJSON(b []byte) error {
	// content 可能是纯字符串（普通 user），先按宽松结构解 role + 原始 content
	var probe struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	m.Role = probe.Role
	if len(probe.Content) > 0 && probe.Content[0] == '"' {
		return json.Unmarshal(probe.Content, &m.ContentPlain)
	}
	return json.Unmarshal(probe.Content, &m.Content)
}

// TestAnthropicStreamErrorEvent 流中途的服务端 error 事件（配额/过载）
// 应以错误返回，不能静默落到「未发 message_stop 视为正常结束」的兜底——
// 否则半截回复被当正常完成、错误伪装成空回复。
func TestAnthropicStreamErrorEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\n" +
			`data: {"type":"message_start","message":{"id":"msg_1"}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"半截"}}` + "\n\n" +
			"event: error\n" +
			`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"))
	}))
	t.Cleanup(srv.Close)

	client, err := NewAnthropicClient(AnthropicOptions{
		APIKey: "test-key", BaseURL: srv.URL, Model: "claude-sonnet-5-5",
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
		t.Fatal("error 事件应导致 StreamChat 返回错误, 不能视为正常完成")
	}
	if !strings.Contains(err.Error(), "Overloaded") {
		t.Errorf("错误应携带服务端信息: %v", err)
	}
	if len(deltas) != 1 || deltas[0] != "半截" {
		t.Errorf("事件前的增量应已送达: %v", deltas)
	}
}
