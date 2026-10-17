package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	wecomaibot "github.com/zhong/wecom-aibot-go"
)

// silentLogger 静默日志器（对齐根包/llm 包测试用法）。
type silentLogger struct{}

func (silentLogger) Debug(string, ...any) {}
func (silentLogger) Info(string, ...any)  {}
func (silentLogger) Warn(string, ...any)  {}
func (silentLogger) Error(string, ...any) {}

// acpWSServer 是 acp 包内的假 openws 服务端（对齐 llm 包 botWSServer 模式）：
// aibot_subscribe ACK 并推送一条文本消息，其余帧一律 ACK。
type acpWSServer struct {
	srv *httptest.Server

	mu     sync.Mutex
	frames []wecomaibot.WsFrameRaw
}

func newACPWSServer(t *testing.T) *acpWSServer {
	t.Helper()
	s := &acpWSServer{}
	upgrader := websocket.Upgrader{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame wecomaibot.WsFrameRaw
			if json.Unmarshal(data, &frame) != nil {
				return
			}
			s.record(frame)
			if strings.HasPrefix(frame.Headers.ReqID, "aibot_subscribe_") {
				_ = conn.WriteJSON(wecomaibot.WsFrameRaw{Headers: wecomaibot.WsHeaders{ReqID: frame.Headers.ReqID}})
				_ = conn.WriteJSON(map[string]any{
					"cmd":     "aibot_msg_callback",
					"headers": map[string]string{"req_id": "msg_req_1"},
					"body": map[string]any{
						"msgid":    "m1",
						"aibotid":  "bot",
						"chatid":   "chat_1",
						"chattype": "group",
						"from":     map[string]string{"userid": "u1"},
						"msgtype":  "text",
						"text":     map[string]string{"content": "hi"},
					},
				})
				continue
			}
			_ = conn.WriteJSON(wecomaibot.WsFrameRaw{Headers: wecomaibot.WsHeaders{ReqID: frame.Headers.ReqID}})
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *acpWSServer) record(frame wecomaibot.WsFrameRaw) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, frame)
}

// recorded 返回已记录帧的副本（供工具面集成测试检查主动发送帧）。
func (s *acpWSServer) recorded() []wecomaibot.WsFrameRaw {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]wecomaibot.WsFrameRaw, len(s.frames))
	copy(out, s.frames)
	return out
}

// streamFrames 依序返回流式回复帧（body.stream 内容非空）。
func (s *acpWSServer) streamFrames() []wecomaibot.StreamPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []wecomaibot.StreamPayload{}
	for _, f := range s.frames {
		if f.Cmd != "aibot_respond_msg" {
			continue
		}
		var body struct {
			Stream wecomaibot.StreamPayload `json:"stream"`
		}
		if json.Unmarshal(f.Body, &body) != nil || body.Stream.ID == "" {
			continue
		}
		out = append(out, body.Stream)
	}
	return out
}

func (s *acpWSServer) url() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http")
}

// connectACPBot 起假服务端 + 客户端并把推送文本消息交给回调。
func connectACPBot(t *testing.T, srv *acpWSServer, run func(bot *wecomaibot.Client, msg wecomaibot.TextMessage)) {
	t.Helper()
	bot, err := wecomaibot.NewClient(wecomaibot.Config{
		BotID:               "bot",
		Secret:              "secret",
		WSURL:               srv.url(),
		HeartbeatIntervalMS: 60000,
		Logger:              silentLogger{},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	bot.OnText(func(_ context.Context, msg wecomaibot.TextMessage) {
		defer func() {
			if rec := recover(); rec != nil {
				t.Errorf("处理回调 panic: %v", rec)
			}
		}()
		run(bot, msg)
	})
	if err := bot.Connect(); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	t.Cleanup(bot.Disconnect)
}

// ---- 假 ACP 适配器（helper process） ----

func TestMain(m *testing.M) {
	if os.Getenv("FAKE_ACP_ADAPTER") == "1" {
		fakeAdapterMain()
		return
	}
	os.Exit(m.Run())
}

// fakeAdapterMain 假适配器主循环：initialize/session/new/session/prompt 各就其位。
// prompt 前（模拟 pi-acp）先推一条 startup info chunk；收到 prompt 后
// 先发 request_permission，得到答复后推送两条正文 chunk 并返回 end_turn。
// prompt 文本带 "TOOLCALL:<tool>" 前缀时（工具面集成测试专用），假适配器
// 转身以 MCP 客户端身份调用 TEST_MCP_URL 端点执行工具，把 prompt 原文、
// 会话 cwd、工具结果一并回显成 chunk——对应现实里 pi 经项目 .pi/mcp.json
// 调用 mcp__wecom__* 工具的行为。
func fakeAdapterMain() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	enc := json.NewEncoder(os.Stdout)

	var sessionReady bool
	var sessionCwd string
	sawPrompt := 0
	for sc.Scan() {
		var req struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.Method == "" || req.ID == nil {
			continue
		}
		switch req.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID,
				"result": map[string]any{"protocolVersion": 1, "authMethods": []any{}}})
		case "session/new":
			sessionReady = true
			if cwd, ok := req.Params["cwd"].(string); ok {
				sessionCwd = cwd
			}
			// startup info：session/new 后、prompt 前推送（对齐 pi-acp 行为）
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update",
				"params": map[string]any{"sessionId": "s_1", "update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       []any{map[string]any{"type": "text", "text": "pi vX.Y startup-info-block"}},
				}}})
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID,
				"result": map[string]any{"sessionId": "s_1"}})
		case "session/prompt":
			sawPrompt++
			promptText := promptTextOf(req.Params)
			if tool, ok := strings.CutPrefix(promptText, "TOOLCALL:"); ok {
				text := fakeMCPInvoke(tool)
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{"sessionId": "s_1", "update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       []any{map[string]any{"type": "text", "text": "ECHO|" + promptText + "|CWD|" + sessionCwd + "|TOOL|" + text}},
					}}})
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID,
					"result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			// 权限请求（首次 prompt 才发）
			if sawPrompt == 1 {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 900, "method": "session/request_permission",
					"params": map[string]any{"sessionId": "s_1", "options": []any{
						map[string]any{"optionId": "allow-1", "kind": "allow_once", "name": "允许"},
						map[string]any{"optionId": "reject-1", "kind": "reject_once", "name": "拒绝"},
					}}})
			}
			// 延迟模式（钉占位帧时序用）：prompt 后静默 FAKE_ADAPTER_DELAY_MS
			// 毫秒才推首 chunk，验证占位帧无需求等首 chunk。
			if d := os.Getenv("FAKE_ADAPTER_DELAY_MS"); d != "" {
				var ms int
				if n, _ := fmt.Sscanf(d, "%d", &ms); n == 1 && ms > 0 {
					time.Sleep(time.Duration(ms) * time.Millisecond)
				}
			}
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update",
				"params": map[string]any{"sessionId": "s_1", "update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       map[string]any{"type": "text", "text": "你好，"},
				}}})
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update",
				"params": map[string]any{"sessionId": "s_1", "update": map[string]any{
					"sessionUpdate": "agent_message_chunk",
					"content":       []any{map[string]any{"type": "text", "text": "张三！"}},
				}}})
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID,
				"result": map[string]any{"stopReason": "end_turn"}})
		}
	}
	_ = sessionReady
}

// promptTextOf 从 session/prompt 参数里解出第一条文本。
func promptTextOf(params map[string]any) string {
	arr, ok := params["prompt"].([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	m, ok := arr[0].(map[string]any)
	if !ok {
		return ""
	}
	text, _ := m["text"].(string)
	return text
}

// fakeMCPInvoke 让假适配器以 MCP 客户端身份调用工具端点（URL/token 经
// 环境变量 TEST_MCP_URL / TEST_MCP_TOKEN 注入）。
func fakeMCPInvoke(tool string) string {
	body := map[string]any{
		"jsonrpc": "2.0", "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{"content": "## 名单\n- 张三"}},
		"id":     1,
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, os.Getenv("TEST_MCP_URL"), bytes.NewReader(b))
	if err != nil {
		return "MCPERR:" + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("TEST_MCP_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "MCPERR:" + err.Error()
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "MCPERR:" + string(data)
	}
	if out.Error != nil {
		return "MCPERR:" + string(data)
	}
	if len(out.Result.Content) == 0 {
		return "MCPERR:无 content 结果"
	}
	return out.Result.Content[0].Text
}

// fakeAdapterConfig 用测试二进制自身当 ACP 适配器。
func fakeAdapterConfig() AgentConfig {
	return fakeAdapterConfigEnv()
}

// fakeAdapterConfigEnv 用测试二进制自身当 ACP 适配器，附加环境变量
// （如 FAKE_ADAPTER_DELAY_MS：prompt 后延迟若干毫秒才推首个正文 chunk，
// 模拟 pi 首chunk 前的 spawn/思考/工具调用静默期）。
func fakeAdapterConfigEnv(extra ...string) AgentConfig {
	return AgentConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMain", "--"},
		Env:     append(os.Environ(), append([]string{"FAKE_ACP_ADAPTER=1"}, extra...)...),
	}
}

// TestRouterEndToEnd 全链路：假 ACP 适配器 → Router → 假企微 WS 服务端。
// 钉四件事：(1) session/new 后的 startup info 不入正文（不渲染给用户）；
// (2) request_permission 无人值守默认拒绝；(3) chunk 变企微流式帧且
// 终帧带全文 finish=true；(4) 两轮消息共享同一 ACP 会话（适配器 sessionId 稳定）。
func TestRouterEndToEnd(t *testing.T) {
	srv := newACPWSServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	finals := make(chan string, 4)
	connectACPBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		router, err := NewRouter(ctx, bot, fakeAdapterConfig())
		if err != nil {
			t.Errorf("NewRouter: %v", err)
			finals <- ""
			return
		}
		const chatKey = "chat_1"
		f1, err := router.Handle(ctx, chatKey, msg, "我叫张三", "sid_1")
		if err != nil {
			t.Errorf("Handle: %v", err)
		}
		f2, err := router.Handle(ctx, chatKey, msg, "我是谁", "sid_2")
		if err != nil {
			t.Errorf("Handle: %v", err)
		}
		if f1 != "你好，张三！" || f2 != "你好，张三！" {
			t.Errorf("终帧正文不对: f1=%q f2=%q", f1, f2)
		}
		finals <- f1 + f2
	})
	select {
	case <-finals:
	case <-time.After(25 * time.Second):
		t.Fatal("等待 Handle 完成超时")
	}
	time.Sleep(200 * time.Millisecond)

	frames := srv.streamFrames()
	if len(frames) == 0 {
		t.Fatal("没有流式帧")
	}
	// startup info 过滤：任何帧正文都不含 startup-info-block
	for _, f := range frames {
		if strings.Contains(f.Content, "startup-info") {
			t.Errorf("startup info 不应渲染给用户: %q", f.Content)
		}
	}
	last := frames[len(frames)-1]
	if !last.Finish || last.Content != "你好，张三！" {
		t.Errorf("终帧应为 finish=true 且带全文: %+v", last)
	}
	// 占位帧在前：首个流式帧应为占位文案且非结束
	if frames[0].Content != DefaultPlaceholder || frames[0].Finish {
		t.Errorf("首帧应为占位帧: %+v", frames[0])
	}
}

// TestPlaceholderBeforeFirstChunk 钉住占位帧时序：假适配器 prompt 后
// 静默 2 秒才推首个正文 chunk（模拟 pi 首 chunk 前 spawn/思考/工具调用
// 的漫长静默期）——静默期内（prompt 尚未返回时）占位帧必须已经落到
// 企微服务端，用户立即看到「正在思考...」而非毫无反应。
func TestPlaceholderBeforeFirstChunk(t *testing.T) {
	srv := newACPWSServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	placeholderSeen := make(chan wecomaibot.StreamPayload, 1)
	connectACPBot(t, srv, func(bot *wecomaibot.Client, msg wecomaibot.TextMessage) {
		router, err := NewRouter(ctx, bot, fakeAdapterConfigEnv("FAKE_ADAPTER_DELAY_MS=2000"))
		if err != nil {
			t.Errorf("NewRouter: %v", err)
			return
		}
		// Handle 内部的占位帧是阻塞发送，返回前必已落服务端；这里在
		// prompt 在途时并发观察帧记录，断言不等首 chunk。
		go func() {
			deadline := time.Now().Add(20 * time.Second)
			for time.Now().Before(deadline) {
				for _, f := range srv.streamFrames() {
					if f.Content == DefaultPlaceholder && !f.Finish {
						select {
						case placeholderSeen <- f:
						default:
						}
						return
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
		if _, err := router.Handle(ctx, "chat_1", msg, "你好", "sid_p"); err != nil {
			t.Errorf("Handle: %v", err)
		}
	})

	// 静默期内（远早于 2 秒延迟结束）占位帧必须已出现
	select {
	case <-placeholderSeen:
	case <-time.After(1500 * time.Millisecond):
		t.Error("首 chunk 静默期内未出现占位帧：占位帧仍在等待首个正文 chunk")
	}
}

// TestAnswerPermissionDefaultReject 权限策略缺省拒绝：无 AllowFunc 时
// 适配器收到 reject_once。
func TestAnswerPermissionDefaultReject(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	proc, err := newAgentProc(ctx, fakeAdapterConfig())
	if err != nil {
		t.Fatalf("newAgentProc: %v", err)
	}
	if err := proc.initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// 与 fake adapter 握一轮 prompt：触发的 request_permission 由
	// answerPermission 自动应答（默认拒绝），prompt 正常收尾即证明
	// 应答闭环没有卡死。
	sid, err := proc.newSessionOnly(ctx, "/tmp")
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	stop, err := proc.prompt(ctx, sid, "你好")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if stop != "end_turn" {
		t.Errorf("stopReason = %q, 期望 end_turn", stop)
	}
}

// TestChunkText 兼容两种 content 形状（v1 数组 / pi-acp 单对象）。
func TestChunkText(t *testing.T) {
	got := chunkText(map[string]any{
		"content": map[string]any{"type": "text", "text": "单对象"},
	})
	if got != "单对象" {
		t.Errorf("单对象形状: %q", got)
	}
	got = chunkText(map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": "数组"},
			map[string]any{"type": "image", "image": "x"},
		},
	})
	if got != "数组" {
		t.Errorf("数组形状: %q", got)
	}
	if got := chunkText(map[string]any{"content": map[string]any{"type": "image"}}); got != "" {
		t.Errorf("非 text 块应返回空: %q", got)
	}
}

// TestChatKeyOf 路由键：群聊用 ChatID，单聊回退 dm:userid。
func TestChatKeyOf(t *testing.T) {
	base := wecomaibot.BaseMessage{ChatID: "chat_9"}
	if got := chatKeyOf(base, "u1"); got != "chat_9" {
		t.Errorf("群聊路由键 = %q", got)
	}
	if got := chatKeyOf(wecomaibot.BaseMessage{}, "u1"); got != "dm:u1" {
		t.Errorf("单聊路由键 = %q", got)
	}
}
