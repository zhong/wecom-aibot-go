package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
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

// silentLogger 静默日志器（对齐根包/llm/acp 包测试用法）。
type silentLogger struct{}

func (silentLogger) Debug(string, ...any) {}
func (silentLogger) Info(string, ...any)  {}
func (silentLogger) Warn(string, ...any)  {}
func (silentLogger) Error(string, ...any) {}

// wsServer 是 mcp 包内的假 openws 服务端（对齐 llm 包 botWSServer 模式）：
// aibot_subscribe ACK 并推送一条文本消息，其余帧一律 ACK；记录全部帧。
type wsServer struct {
	srv *httptest.Server

	mu     sync.Mutex
	frames []wecomaibot.WsFrameRaw
}

func newWSServer(t *testing.T) *wsServer {
	t.Helper()
	s := &wsServer{}
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
			s.mu.Lock()
			s.frames = append(s.frames, frame)
			s.mu.Unlock()
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

func (s *wsServer) url() string {
	return "ws" + strings.TrimPrefix(s.srv.URL, "http")
}

// sendMsgFrames 返回主动发送帧（aibot_send_msg）。
func (s *wsServer) sendMsgFrames() []wecomaibot.WsFrameRaw {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []wecomaibot.WsFrameRaw
	for _, f := range s.frames {
		if f.Cmd == "aibot_send_msg" {
			out = append(out, f)
		}
	}
	return out
}

// templateCardFrames 返回卡片帧并解出 card 内容。
func (s *wsServer) templateCardFrames() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, f := range s.frames {
		if f.Cmd != "aibot_respond_msg" {
			continue
		}
		var body struct {
			MsgType      string         `json:"msgtype"`
			TemplateCard map[string]any `json:"template_card"`
		}
		if json.Unmarshal(f.Body, &body) != nil || body.MsgType != "template_card" {
			continue
		}
		out = append(out, body.TemplateCard)
	}
	return out
}

// testEnv 是一个就绪的测试环境：假企微 WS 服务端 + 已连接的真客户端 +
// 已监听的 MCP 服务端 + 一条群聊文本消息载体。
type testEnv struct {
	ws  *wsServer
	bot *wecomaibot.Client
	mcp *WeComServer
	msg wecomaibot.TextMessage
}

// newTestEnv 起全套环境。msg 载体直接构造字面量（chat_1 群聊文本消息，
// 对齐假服务端推送的消息形状）。
func newTestEnv(t *testing.T, token string) *testEnv {
	t.Helper()
	ws := newWSServer(t)
	bot, err := wecomaibot.NewClient(wecomaibot.Config{
		BotID:               "bot",
		Secret:              "secret",
		WSURL:               ws.url(),
		HeartbeatIntervalMS: 60000,
		Logger:              silentLogger{},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	if err := bot.Connect(); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	// Connect 是异步拨号：立即发起 tools/call 会与拨号竞速（SendMessage
	// 需要 WS 就绪）。等待连接就绪再继续，消除偶发 ErrNotConnected。
	deadline := time.Now().Add(5 * time.Second)
	for !bot.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("等待 WS 连接就绪超时")
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(bot.Disconnect)

	s, err := NewWeComServer(bot, token)
	if err != nil {
		t.Fatalf("NewWeComServer: %v", err)
	}
	if err := s.Start(DefaultAddr); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	msg := wecomaibot.TextMessage{}
	msg.ReqID = "msg_req_1"
	msg.ChatID = "chat_1"
	msg.Text.Content = "hi"
	return &testEnv{ws: ws, bot: bot, mcp: s, msg: msg}
}

// mcpClient 是测试用极简 MCP HTTP 客户端（对齐 pi-mcp 线格式）。
type mcpClient struct {
	base  string
	token string
	http  *http.Client
}

func (c *mcpClient) post(t *testing.T, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, c.base, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &out)
	}
	return resp.StatusCode, out
}

func (c *mcpClient) call(t *testing.T, id int, method string, params any) map[string]any {
	t.Helper()
	code, out := c.post(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if code != http.StatusOK {
		t.Fatalf("POST %s: status=%d body=%v", method, code, out)
	}
	return out
}

// toolText 取工具结果 content[0].text。
func toolText(t *testing.T, result map[string]any) string {
	t.Helper()
	res, _ := result["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("结果无 content: %v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

// clientFor 返回打指定 chatKey 端点的客户端。
func (e *testEnv) clientFor(chatKey string) *mcpClient {
	return &mcpClient{base: e.mcp.URLFor(chatKey), token: e.mcp.Token(), http: &http.Client{}}
}

// runInChat 登记快照并执行一次工具调用（模拟 agent 在 prompt 在途期间的行为），
// 返回工具结果文本。
func (e *testEnv) runInChat(t *testing.T, chatKey string, req wecomaibot.RequestCarrier, tool, args string) string {
	t.Helper()
	return e.runInChatSave(t, chatKey, req, tool, args, "")
}

// runInChatSave 同 runInChat 但带附件落盘目录（saveDir 传给 SetChat，
// read_attachment save=true 时二进制附件写到该目录）。
func (e *testEnv) runInChatSave(t *testing.T, chatKey string, req wecomaibot.RequestCarrier, tool, args, saveDir string) string {
	t.Helper()
	e.mcp.SetChat(chatKey, req, saveDir)
	defer e.mcp.ClearChat(chatKey)
	c := e.clientFor(chatKey)
	var arguments any
	_ = json.Unmarshal([]byte(args), &arguments)
	out := c.call(t, 1, "tools/call", map[string]any{"name": tool, "arguments": arguments})
	if errObj, ok := out["error"]; ok {
		t.Fatalf("tools/call 协议错误: %v", errObj)
	}
	return toolText(t, out)
}

// ---- 线格式测试 ----

func TestInitializeEchoesSupportedVersion(t *testing.T) {
	e := newTestEnv(t, "tok_1")
	c := e.clientFor("chat_1")
	for _, v := range supportedVersions {
		out := c.call(t, 1, "initialize", map[string]any{"protocolVersion": v})
		res, _ := out["result"].(map[string]any)
		if res["protocolVersion"] != v {
			t.Errorf("protocolVersion %s 应回显: %v", v, res["protocolVersion"])
		}
	}
	out := c.call(t, 2, "initialize", map[string]any{"protocolVersion": "1999-01-01"})
	res, _ := out["result"].(map[string]any)
	if res["protocolVersion"] != supportedVersions[0] {
		t.Errorf("未知版本应回最新: %v", res["protocolVersion"])
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("capabilities 应声明 tools: %v", caps)
	}
}

func TestToolsListExposesThreeTools(t *testing.T) {
	e := newTestEnv(t, "tok_1")
	out := e.clientFor("chat_1").call(t, 1, "tools/list", map[string]any{})
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("应暴露 3 个工具, 实际 %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		tm, _ := tl.(map[string]any)
		names[tm["name"].(string)] = true
		if _, ok := tm["inputSchema"]; !ok {
			t.Errorf("工具 %v 应有 inputSchema 字段", tm["name"])
		}
	}
	for _, want := range []string{"send_template_card", "send_markdown", "read_attachment"} {
		if !names[want] {
			t.Errorf("缺少工具 %s", want)
		}
	}
}

func TestNotificationInitializedAndGet405AndPing(t *testing.T) {
	e := newTestEnv(t, "tok_1")
	c := e.clientFor("chat_1")

	// 通知（无 id）→ 202
	code, _ := c.post(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if code != http.StatusAccepted {
		t.Errorf("initialized 通知应 202, 实际 %d", code)
	}

	// GET SSE 探测 → 405（pi-mcp 视为无服务端流，正常降级）
	req, _ := http.NewRequest(http.MethodGet, c.base, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("GET 失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET 应 405, 实际 %d", resp.StatusCode)
	}

	// ping → {}
	out := c.call(t, 3, "ping", map[string]any{})
	if _, ok := out["result"]; !ok {
		t.Errorf("ping 应答 result: %v", out)
	}
}

func TestAuthorizationRequired(t *testing.T) {
	e := newTestEnv(t, "tok_1")
	// 无 token
	c0 := &mcpClient{base: e.mcp.URLFor("chat_1"), http: &http.Client{}}
	code, _ := c0.post(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	if code != http.StatusUnauthorized {
		t.Errorf("无 token 应 401, 实际 %d", code)
	}
	// 错 token
	c1 := &mcpClient{base: e.mcp.URLFor("chat_1"), token: "wrong", http: &http.Client{}}
	code, _ = c1.post(t, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	if code != http.StatusUnauthorized {
		t.Errorf("错 token 应 401, 实际 %d", code)
	}
}

func TestUnknownMethodAndTool(t *testing.T) {
	e := newTestEnv(t, "tok_1")
	c := e.clientFor("chat_1")
	out := c.call(t, 1, "resources/list", map[string]any{})
	errObj, _ := out["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32601 {
		t.Errorf("未知方法应 -32601: %v", out)
	}
	if id, _ := out["id"].(float64); id != 1 {
		t.Errorf("id 应回传: %v", out["id"])
	}

	out = c.call(t, 2, "tools/call", map[string]any{"name": "no_such_tool", "arguments": map[string]any{}})
	errObj, _ = out["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32602 {
		t.Errorf("未知工具应 -32602: %v", out)
	}
}

func TestSessionSetupBeforeFirstMessage(t *testing.T) {
	// pi 在 session/new 拉起时（早于首条消息的 SetChat）就会连接端点握手：
	// 注册表尚无快照时 initialize / tools/list / ping 必须照常成功，
	// 只有 tools/call 才依赖在途快照。
	e := newTestEnv(t, "tok_1")
	c := e.clientFor("chat_9")
	out := c.call(t, 1, "initialize", map[string]any{"protocolVersion": supportedVersions[0]})
	if _, ok := out["result"]; !ok {
		t.Errorf("initialize 应成功: %v", out)
	}
	out = c.call(t, 2, "tools/list", map[string]any{})
	if _, ok := out["result"]; !ok {
		t.Errorf("tools/list 应成功: %v", out)
	}
	out = c.call(t, 3, "ping", map[string]any{})
	if _, ok := out["result"]; !ok {
		t.Errorf("ping 应成功: %v", out)
	}
}

func TestNonMCPPath404(t *testing.T) {
	// 非 /mcp/<key> 路径 → 404。
	e := newTestEnv(t, "tok_1")
	req, _ := http.NewRequest(http.MethodPost, e.mcp.URL()+"/other", nil)
	req.Header.Set("Authorization", "Bearer "+e.mcp.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("非 mcp 路径应 404, 实际 %d", resp.StatusCode)
	}
}

// ---- 工具面执行 E2E（真 executor + 假企微 WS） ----

func TestToolsCallSendMarkdown(t *testing.T) {
	e := newTestEnv(t, "")
	result := e.runInChat(t, "chat_1", e.msg, "send_markdown", `{"content":"## 名单\n- 张三"}`)
	if !strings.Contains(result, `"ok":true`) {
		t.Errorf("执行结果应为 ok: %s", result)
	}
	time.Sleep(200 * time.Millisecond)
	frames := e.ws.sendMsgFrames()
	if len(frames) != 1 {
		t.Fatalf("应有一条主动发送帧, 实际 %d", len(frames))
	}
	var body struct {
		ChatID  string `json:"chatid"`
		MsgType string `json:"msgtype"`
	}
	if err := json.Unmarshal(frames[0].Body, &body); err != nil {
		t.Fatalf("解析发送帧失败: %v", err)
	}
	if body.ChatID != "chat_1" || body.MsgType != "markdown" {
		t.Errorf("markdown 主动发送帧不对: %+v", body)
	}
}

func TestToolsCallSendTemplateCard(t *testing.T) {
	e := newTestEnv(t, "")
	result := e.runInChat(t, "chat_1", e.msg, "send_template_card",
		`{"card":{"card_type":"text_notice","main_title":{"title":"t"}}}`)
	if !strings.Contains(result, `"ok":true`) {
		t.Errorf("执行结果应为 ok: %s", result)
	}
	time.Sleep(200 * time.Millisecond)
	cards := e.ws.templateCardFrames()
	if len(cards) != 1 || cards[0]["card_type"] != "text_notice" {
		t.Errorf("应发出一张卡片: %+v", cards)
	}
}

func TestToolsCallReadAttachment(t *testing.T) {
	e := newTestEnv(t, "")
	// 携带附件的 FileMessage 载体（本地 httptest 下载源），下载走 HTTP。
	file, _ := os.CreateTemp(t.TempDir(), "名单-*.txt")
	_, _ = file.WriteString("张三,李四,王五")
	_ = file.Close()
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := os.ReadFile(file.Name())
		_, _ = w.Write(b)
	}))
	t.Cleanup(dlSrv.Close)

	fmsg := wecomaibot.FileMessage{}
	fmsg.ReqID = "req_1"
	fmsg.ChatID = "chat_1"
	fmsg.File.URL = dlSrv.URL
	result := e.runInChat(t, "chat_1", fmsg, "read_attachment", `{}`)
	var parsed struct {
		Ok      bool   `json:"ok"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("解析结果失败: %v", err)
	}
	if !parsed.Ok || parsed.Type != "text" || !strings.Contains(parsed.Content, "张三") {
		t.Errorf("文本附件应返回内容预览: %s", result)
	}
}

func TestToolsCallDMChatKey(t *testing.T) {
	// 单聊键 dm:u1 含冒号：URL 编码后端点与注册表仍按原键定位；
	// DM 无 chatID，send_markdown 回退普通回复帧（req 非空）。
	e := newTestEnv(t, "")
	dmMsg := wecomaibot.TextMessage{}
	dmMsg.ReqID = "req_dm_1"
	dmMsg.From.UserID = "u1"
	dmMsg.Text.Content = "hi"
	result := e.runInChat(t, "dm:u1", dmMsg, "send_markdown", `{"content":"**加粗**"}`)
	if !strings.Contains(result, `"ok":true`) {
		t.Errorf("单聊执行结果应为 ok: %s", result)
	}
}

func TestToolsCallNoSnapshotSelfCorrects(t *testing.T) {
	// 会话没有在途消息（快照未登记/已清除）：返回 ok:false 的正常结果，
	// 不是协议错误——模型可自纠。
	e := newTestEnv(t, "")
	c := e.clientFor("chat_1")
	out := c.call(t, 1, "tools/call", map[string]any{
		"name": "send_markdown", "arguments": map[string]any{"content": "x"},
	})
	if errObj, ok := out["error"]; ok {
		t.Fatalf("未命中快照不应是协议错误: %v", errObj)
	}
	text := toolText(t, out)
	if !strings.Contains(text, `"ok":false`) || !strings.Contains(text, "没有进行中的消息") {
		t.Errorf("应返回可自纠的 ok:false 结果: %s", text)
	}
}

// TestChatKeyEscapeRoundTrip 路径编码往返（dm: 键含冒号）。
func TestChatKeyEscapeRoundTrip(t *testing.T) {
	for _, key := range []string{"chat_1", "dm:u1", "dm:中文用户"} {
		esc := chatKeyEscape(key)
		if strings.ContainsAny(esc, "/") {
			t.Errorf("编码后不应含裸斜杠: %q", esc)
		}
		got, err := chatKeyUnescape(esc)
		if err != nil || got != key {
			t.Errorf("往返失败: %q → %q → %q (err=%v)", key, esc, got, err)
		}
	}
}

// TestSetAdvertiseBase 通告基址覆盖：容器化 agent 部署里监听回环、
// 通告容器视角可达地址（host.docker.internal / 容器网关），URLFor 写进
// agent 配置的端点才真正可达。不带 scheme 自动补 http。
func TestSetAdvertiseBase(t *testing.T) {
	e := newTestEnv(t, "")
	// 缺省：通告 = 实际监听地址
	if got, want := e.mcp.URL(), "http://"+e.mcp.Addr(); got != want {
		t.Errorf("缺省 URL = %q, 期望 %q", got, want)
	}
	e.mcp.SetAdvertiseBase("host.docker.internal:8080")
	if got := e.mcp.URL(); got != "http://host.docker.internal:8080" {
		t.Errorf("通告 URL = %q, 期望 http://host.docker.internal:8080", got)
	}
	if got := e.mcp.URLFor("chat_1"); got != "http://host.docker.internal:8080/mcp/chat_1" {
		t.Errorf("URLFor = %q, 期望通告基址拼会话路径", got)
	}
	// 显式 scheme 原样保留
	e.mcp.SetAdvertiseBase("https://gw.example")
	if got := e.mcp.URL(); got != "https://gw.example" {
		t.Errorf("显式 scheme URL = %q, 期望原样", got)
	}
	// 通告不影响实际监听（请求仍落在真实端口）
	e.mcp.SetAdvertiseBase("host.docker.internal:8080")
	if _, err := net.Dial("tcp", e.mcp.Addr()); err != nil {
		t.Errorf("实际监听地址不可达: %v", err)
	}
	// clientFor 走通告地址打不通（本机没有该 DNS 场景），但直打真实端口
	// 带鉴权 tools/list 正常——通告只影响写进配置的字符串，不影响服务。
	u := "http://" + e.mcp.Addr() + "/mcp/chat_1"
	c := &mcpClient{base: u, token: e.mcp.Token(), http: &http.Client{}}
	out := c.call(t, 1, "tools/list", map[string]any{})
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) == 0 {
		t.Errorf("tools/list 空: %v", out)
	}
}

// TestToolsCallReadAttachmentSave 通过 saveDir（会话 cwd）+ save=true 的
// read_attachment：二进制附件落盘到 saveDir，结果返回 saved 绝对路径，
// agent 可用视觉/本地工具直接读取。
func TestToolsCallReadAttachmentSave(t *testing.T) {
	e := newTestEnv(t, "")
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	var pngLen int
	dlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="photo.png"`)
		pngLen, _ = w.Write(png)
	}))
	t.Cleanup(dlSrv.Close)

	fmsg := wecomaibot.FileMessage{}
	fmsg.ReqID = "req_save_1"
	fmsg.ChatID = "chat_1"
	fmsg.File.URL = dlSrv.URL
	saveDir := t.TempDir()
	result := e.runInChatSave(t, "chat_1", fmsg, "read_attachment", `{"save":true}`, saveDir)
	var parsed struct {
		Ok    bool   `json:"ok"`
		Type  string `json:"type"`
		Saved string `json:"saved"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("解析结果失败: %v（%s）", err, result)
	}
	if !parsed.Ok || parsed.Saved == "" {
		t.Fatalf("save=true 应返回落盘路径: %s", result)
	}
	if strings.Contains(parsed.Saved, "..") {
		t.Fatalf("saved 路径不应越出 saveDir: %s", parsed.Saved)
	}
	buf, err := os.ReadFile(parsed.Saved)
	if err != nil || pngLen == 0 || len(buf) != pngLen {
		t.Fatalf("落盘文件应可读且字节一致: err=%v len=%d", err, len(buf))
	}
}

// ---- 外发频控 E2E（SetSendMinInterval + tools/call 闭环） ----

// TestSendRateLimitBlocksSecondBurst 验证频控对注入滥用形态的拦截：
// 窗口内第二次 send_markdown 被 ok:false 拒绝且不产生第二条主动发送帧，
// 等待文案随剩余时长返回（模型可自纠）。
func TestSendRateLimitBlocksSecondBurst(t *testing.T) {
	e := newTestEnv(t, "")
	e.mcp.SetSendMinInterval(10 * time.Second)

	first := e.runInChat(t, "chat_1", e.msg, "send_markdown", `{"content":"第一条"}`)
	if !strings.Contains(first, `"ok":true`) {
		t.Fatalf("首发应放行: %s", first)
	}
	second := e.runInChat(t, "chat_1", e.msg, "send_markdown", `{"content":"第二条"}`)
	if !strings.Contains(second, `"ok":false`) || !strings.Contains(second, "发送过于频繁") {
		t.Fatalf("窗口内第二次应被频控拒绝: %s", second)
	}
	time.Sleep(200 * time.Millisecond)
	frames := e.ws.sendMsgFrames()
	if len(frames) != 1 {
		t.Fatalf("被限请求不应产生第二条发送帧, 实际 %d 条", len(frames))
	}
}

// TestSendRateLimitCrossChatIndependent 验证频控按会话隔离：
// chat_1 被限不影响 chat_2 首发。
func TestSendRateLimitCrossChatIndependent(t *testing.T) {
	e := newTestEnv(t, "")
	e.mcp.SetSendMinInterval(10 * time.Second)

	msg2 := e.msg
	msg2.ChatID = "chat_2"

	if out := e.runInChat(t, "chat_1", e.msg, "send_markdown", `{"content":"a"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("chat_1 首发应放行: %s", out)
	}
	if out := e.runInChat(t, "chat_2", msg2, "send_markdown", `{"content":"b"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("chat_2 应独立放行: %s", out)
	}
}

// TestSendRateLimitFailedSendNotConsumed 验证发送失败不消耗频控额度：
// 失败（无法定位会话的载体）后立刻重试合法发送仍放行。
func TestSendRateLimitFailedSendNotConsumed(t *testing.T) {
	e := newTestEnv(t, "")
	e.mcp.SetSendMinInterval(10 * time.Second)

	// 参数校验失败（content 为空）：未触达发送面，应 ok:false 且不消耗额度。
	failed := e.runInChat(t, "chat_1", e.msg, "send_markdown", `{"content":""}`)
	if !strings.Contains(failed, `"ok":false`) {
		t.Fatalf("空 content 应执行失败: %s", failed)
	}
	// 失败已 release：同会话紧接着的合法发送不再被挡。
	if out := e.runInChat(t, "chat_1", e.msg, "send_markdown", `{"content":"ok one"}`); !strings.Contains(out, `"ok":true`) {
		t.Fatalf("失败的发送不应消耗额度: %s", out)
	}
}

// TestSendRateLimitDisabledByZero 频控置 0 等价关闭：连发全放行。
func TestSendRateLimitDisabledByZero(t *testing.T) {
	e := newTestEnv(t, "")
	e.mcp.SetSendMinInterval(0)

	for i := 0; i < 3; i++ {
		if out := e.runInChat(t, "chat_1", e.msg, "send_markdown", `{"content":"burst"}`); !strings.Contains(out, `"ok":true`) {
			t.Fatalf("频控关闭后第 %d 次应放行: %s", i, out)
		}
	}
}

// TestSendRateLimitNotAffectReadAttachment 频控只作用于外发类工具：
// read_attachment 只读不受限（连发多次都正常执行）。
func TestSendRateLimitNotAffectReadAttachment(t *testing.T) {
	e := newTestEnv(t, "")
	e.mcp.SetSendMinInterval(10 * time.Second)

	fmsg := wecomaibot.FileMessage{}
	fmsg.ReqID = "msg_req_f"
	fmsg.ChatID = "chat_1"
	fmsg.File.URL = "http://127.0.0.1:1/a.txt" // 下载会失败，但工具正常执行返回结果

	for i := 0; i < 3; i++ {
		out := e.runInChat(t, "chat_1", fmsg, "read_attachment", `{}`)
		if strings.Contains(out, "发送过于频繁") {
			t.Fatalf("read_attachment 不应被频控: %s", out)
		}
	}
}

// TestChatKeyEscapeHTTPRoundTrip 含 % 的 chatKey 经 HTTP 路由不得与
// 转义后的其他键坍缩到同一端点：r.URL.Path 已被 net/http 解码一次，
// 服务端再解码就成双重解码——"a%2Fb" 与 "a/b" 两个不同的键会被折
// 到同一个会话（纯函数往返测试穿不过 HTTP 路由层，须整层钉）。
func TestChatKeyEscapeHTTPRoundTrip(t *testing.T) {
	e := newTestEnv(t, "")
	e.mcp.SetChat("a%2Fb", e.msg, "")
	defer e.mcp.ClearChat("a%2Fb")
	// c1 用 tools/call 断言注册表命中（ping 不查注册表，判别不了路由）。
	c1 := e.clientFor("a%2Fb")
	out1 := c1.call(t, 1, "tools/call", map[string]any{
		"name": "read_attachment", "arguments": map[string]any{},
	})
	res1, _ := out1["result"].(map[string]any)
	text1 := ""
	if content, ok := res1["content"].([]any); ok && len(content) > 0 {
		if m, ok := content[0].(map[string]any); ok {
			text1, _ = m["text"].(string)
		}
	}
	if strings.Contains(text1, "没有进行中的消息") {
		t.Fatalf(`"a%%2Fb" 已登记快照, 应命中注册表(双重解码会把它误译成别的键): %s`, text1)
	}
	// "a/b" 是不同的键：PathEscape 后是 "a%2Fb" 的转义形态——两者在
	// 服务端必须解析回各自的原键，不得坍缩。
	c2 := &mcpClient{base: e.mcp.URLFor("a/b"), token: e.mcp.Token(), http: &http.Client{}}
	out2 := c2.call(t, 1, "tools/call", map[string]any{
		"name": "send_markdown", "arguments": map[string]any{"content": "x"},
	})
	res, _ := out2["result"].(map[string]any)
	text := ""
	if content, ok := res["content"].([]any); ok && len(content) > 0 {
		if m, ok := content[0].(map[string]any); ok {
			text, _ = m["text"].(string)
		}
	}
	if !strings.Contains(text, "没有进行中的消息") {
		t.Errorf(`"a/b" 键未登记快照, 应答 ok:false 自纠; 实际: %s`, text)
	}
}

// TestSendRateLimitSnapshotMissNotConsumed 快照未命中的外发调用不应
// 消耗频控额度：agent 在 prompt 窗口外补发的调用（如 ask 刚返回、
// ClearChat 已执行）返回 ok:false 自纠，但这次空转不能挡住紧接着的
// 合法发送（旧实现在 miss 分支早退，不做 release）。
func TestSendRateLimitSnapshotMissNotConsumed(t *testing.T) {
	e := newTestEnv(t, "")
	e.mcp.SetSendMinInterval(10 * time.Second)

	// 不登记快照直接调端点：ok:false 自纠。
	c := e.clientFor("chat_7")
	out := c.call(t, 1, "tools/call", map[string]any{
		"name": "send_markdown", "arguments": map[string]any{"content": "x"},
	})
	res, _ := out["result"].(map[string]any)
	text := ""
	if content, ok := res["content"].([]any); ok && len(content) > 0 {
		if m, ok := content[0].(map[string]any); ok {
			text, _ = m["text"].(string)
		}
	}
	if !strings.Contains(text, "没有进行中的消息") {
		t.Fatalf("快照未命中应 ok:false 自纠: %s", text)
	}
	// 紧接着的合法发送（登记快照后）不被空转调用挡住。
	if got := e.runInChat(t, "chat_7", e.msg, "send_markdown", `{"content":"ok"}`); !strings.Contains(got, `"ok":true`) {
		t.Fatalf("miss 调用不应消耗频控额度: %s", got)
	}
}

func TestParseErrorBadJSON(t *testing.T) {
	// 请求体不可解析 → id 为 null 的 -32700 错误（MCP 契约：解析失败时
	// id 不可知，必须回 null 而不是回传）。writeJSONRPCError 的唯一触发面。
	e := newTestEnv(t, "tok_1")
	req, _ := http.NewRequest(http.MethodPost, e.mcp.URLFor("chat_1"),
		strings.NewReader("{这不是 JSON"))
	req.Header.Set("Authorization", "Bearer "+e.mcp.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST 失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("坏请求体应 200 带协议错, 实际 %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("应答应为 JSON: %v", err)
	}
	if out["id"] != nil {
		t.Errorf("解析失败 id 必须为 null: %v", out["id"])
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj == nil || errObj["code"].(float64) != -32700 {
		t.Errorf("坏 JSON 应 -32700: %v", out)
	}
	if out["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc 字段缺失: %v", out)
	}
}
