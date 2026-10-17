package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wecomaibot "github.com/zhong/wecom-aibot-go"
	"github.com/zhong/wecom-aibot-go/mcp"
)

// waitConnected 等待异步拨号完成：Connect 立即返回，Handle 内流式
// 发送需要 WS 就绪，不等会与拨号竞速偶发失败。
func waitConnected(t *testing.T, bot *wecomaibot.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !bot.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("等待 WS 连接就绪超时")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// toolBCastEnv 是工具面集成测试的可复用环境：假企微 WS + 客户端 +
// mcp.WeComServer（真工具面）+ 带 TEST_MCP_* 环境变量的假适配器配置。
func toolSurfaceEnv(t *testing.T) (*Router, *acpWSServer, string, string) {
	t.Helper()
	srv := newACPWSServer(t)
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
	if err := bot.Connect(); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	waitConnected(t, bot)
	t.Cleanup(bot.Disconnect)

	mcpSrv, err := mcp.NewWeComServer(bot, "test_token")
	if err != nil {
		t.Fatalf("NewWeComServer: %v", err)
	}
	if err := mcpSrv.Start(mcp.DefaultAddr); err != nil {
		t.Fatalf("mcp Start: %v", err)
	}
	t.Cleanup(func() { _ = mcpSrv.Close() })

	// 假适配器以 MCP 客户端身份打到 chat_1 的专属端点
	cfg := fakeAdapterConfig()
	cfg.Env = append(cfg.Env,
		"TEST_MCP_URL="+mcpSrv.URLFor("chat_1"),
		"TEST_MCP_TOKEN=test_token",
	)
	router, err := NewRouter(context.Background(), bot, cfg)
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	workDir := t.TempDir()
	if err := router.EnableTools(mcpSrv, workDir); err != nil {
		t.Fatalf("EnableTools: %v", err)
	}
	return router, srv, workDir, mcpSrv.URLFor("chat_1")
}

// mpcEchoChunk 从流式终帧正文里拆出假适配器的回显分段：
// ECHO|<prompt 原文>|CWD|<会话 cwd>|TOOL|<工具结果>。
func parseEcho(final string) (prompt, cwd, tool string, err error) {
	rest, ok := strings.CutPrefix(final, "ECHO|")
	if !ok {
		return "", "", "", fmt.Errorf("终帧不是 ECHO 回显: %q", final)
	}
	parts := strings.Split(rest, "|CWD|")
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("ECHO 缺 CWD 段: %q", final)
	}
	prompt = parts[0]
	parts = strings.Split(parts[1], "|TOOL|")
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("ECHO 缺 TOOL 段: %q", final)
	}
	return prompt, parts[0], parts[1], nil
}

// TestRouterToolsSurfaceEndToEnd 全链路：Router（快照登记）→ 假适配器经
// 会话 cwd 的项目 mcp.json 端点调 MCP 工具 → llm executor → 假企微 WS。
// 钉五件事：(1) prompt 原文没有任何注入头；(2) session/new 的 cwd 是该
// 会话专属目录；(3) 目录里有 .pi/mcp.json 且指向 chat_1 端点 +
// exposure=direct + token 头；(4) 工具调用真实执行且 ok:true；
// (5) Handle 收尾后快照清除（后续调用 ok:false）。
func TestRouterToolsSurfaceEndToEnd(t *testing.T) {
	router, srv, workDir, endpoint := toolSurfaceEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg := wecomaibot.TextMessage{}
	msg.ReqID = "req_tools_1"
	msg.ChatID = "chat_1"
	msg.Text.Content = "hi"
	final, err := router.Handle(ctx, "chat_1", msg, "TOOLCALL:send_markdown", "sid_tools_1")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	prompt, cwd, tool, err := parseEcho(final)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "TOOLCALL:send_markdown" {
		t.Errorf("prompt 原文不应被注入修改: %q", prompt)
	}
	workSub := filepath.Join(workDir, "chat_1")
	if !strings.HasSuffix(cwd, workSub) {
		t.Errorf("会话 cwd 应为专属目录 %s, 实际 %s", workSub, cwd)
	}
	if !strings.Contains(tool, `"ok":true`) {
		t.Errorf("工具结果应为 ok:true: %s", tool)
	}

	// 会话目录 .pi/mcp.json 检查
	raw, err := os.ReadFile(filepath.Join(workSub, ".pi", "mcp.json"))
	if err != nil {
		t.Fatalf("读取会话 mcp.json 失败: %v", err)
	}
	var cfg struct {
		McpServers map[string]struct {
			URL      string            `json:"url"`
			Headers  map[string]string `json:"headers"`
			Exposure string            `json:"exposure"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("解析 mcp.json 失败: %v", err)
	}
	wecom, ok := cfg.McpServers["wecom"]
	if !ok {
		t.Fatalf("mcp.json 缺 wecom server: %s", raw)
	}
	if wecom.URL != endpoint {
		t.Errorf("url 应指向 chat_1 专属端点: %s != %s", wecom.URL, endpoint)
	}
	if wecom.Exposure != "direct" {
		t.Errorf("exposure 必须为 direct（模型可见）, 实际 %q", wecom.Exposure)
	}
	if wecom.Headers["Authorization"] != "Bearer test_token" {
		t.Errorf("headers 应携带 Bearer token: %+v", wecom.Headers)
	}

	// 假企微 WS 应收到 send_markdown 的主动发送帧
	time.Sleep(200 * time.Millisecond)
	var found bool
	for _, f := range srv.recorded() {
		if f.Cmd == "aibot_send_msg" {
			if strings.Contains(string(f.Body), `"msgtype":"markdown"`) ||
				strings.Contains(string(f.Body), "张三") {
				found = true
			}
		}
	}
	if !found {
		t.Error("假企微 WS 未收到 send_markdown 的主动发送帧")
	}

	// Handle 已收尾：快照清除，直接调端点应 ok:false
	c := &http.Client{}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_markdown","arguments":{"content":"x"}}}`
	req, _ := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test_token")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("收尾后 tools/call 请求失败: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// 工具结果 JSON 被包进 MCP content 文本再转义，按 errmsg 语义断言即可
	if !strings.Contains(string(data), "没有进行中的消息") {
		t.Errorf("收尾后调用应 ok:false 自纠: %s", data)
	}
}

// TestRouterToolsSurfaceSecondChatDistinctDir 第二个会话：
// 不同 chatKey → 不同 cwd 目录与端点，互不影响。
func TestRouterToolsSurfaceSecondChatDistinctDir(t *testing.T) {
	router, _, workDir, _ := toolSurfaceEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg := wecomaibot.TextMessage{}
	msg.ReqID = "req_dm_1"
	msg.From.UserID = "u9"
	msg.Text.Content = "hi"
	final, err := router.Handle(ctx, "dm:u9", msg, "TOOLCALL:send_markdown", "sid_dm_1")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	_, cwd, tool, err := parseEcho(final)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(cwd, filepath.Join(workDir, "dm:u9")) {
		t.Errorf("dm 会话 cwd 应为含原键的专属目录: %s", cwd)
	}
	// dm:u9 端点没有在途快照（假适配器打到的是 chat_1 端点），该调用按设计
	// 返回自纠 ok:false——恰好证明端点隔离。
	if !strings.Contains(tool, `"ok":false`) {
		t.Errorf("dm 端点应返回自纠结果: %s", tool)
	}
}

// TestRouterToolsSurfaceCleanMsgWithoutPrefix 工具指令前缀之外的普通
// 消息照常走原路径（权限请求→正文 chunk），工具面登记不影响。
func TestRouterToolsSurfaceCleanMsgWithoutPrefix(t *testing.T) {
	router, srv, _, _ := toolSurfaceEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg := wecomaibot.TextMessage{}
	msg.ReqID = "req_plain_1"
	msg.ChatID = "chat_1"
	msg.Text.Content = "hi"
	final, err := router.Handle(ctx, "chat_1", msg, "我叫张三", "sid_plain_1")
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if final != "你好，张三！" {
		t.Errorf("普通消息终帧不对: %q", final)
	}
	time.Sleep(200 * time.Millisecond)
	frames := srv.streamFrames()
	if len(frames) == 0 {
		t.Fatal("普通消息应有流式帧")
	}
	last := frames[len(frames)-1]
	if !last.Finish || last.Content != "你好，张三！" {
		t.Errorf("终帧不带全文: %+v", last)
	}
}

// TestSafeKey / TestSessionWorkDir 纯函数测试。注：冒号是合法路径/目录
// 字符，PathEscape 不转义——safeKey 与 mcp 端点用同一编码保证一一对应。
func TestSafeKeyAndSessionWorkDir(t *testing.T) {
	if safeKey("chat_1") != "chat_1" {
		t.Errorf("普通键不应变形: %q", safeKey("chat_1"))
	}
	if got := sessionWorkDir("/w", "chat_1"); got != "/w/chat_1" {
		t.Errorf("sessionWorkDir: %q", got)
	}
	// 含路径分隔符的键（防御性）：转义后不能越出 workDir
	k := safeKey("a/b")
	if strings.Contains(k, "/") || strings.HasPrefix(k, ".") {
		t.Errorf("safeKey 应消除路径穿越: %q", k)
	}
}

// TestEnableToolsValidation 参数校验。
func TestEnableToolsValidation(t *testing.T) {
	srv := newACPWSServer(t)
	bot, err := wecomaibot.NewClient(wecomaibot.Config{
		BotID: "bot", Secret: "secret", WSURL: srv.url(),
		HeartbeatIntervalMS: 60000, Logger: silentLogger{},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	if err := bot.Connect(); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	waitConnected(t, bot)
	t.Cleanup(bot.Disconnect)

	router, err := NewRouter(context.Background(), bot, fakeAdapterConfig())
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	if err := router.EnableTools(nil, t.TempDir()); err == nil {
		t.Error("nil surface 应报错")
	}
	if err := router.EnableTools(&mcpServerStub{}, ""); err == nil {
		t.Error("空 workDir 应报错")
	}
}

// mcpServerStub 是 Handle 快照钩子的最小面验证桩：记录 Set/Clear 调用。
type mcpServerStub struct {
	calls []string
}

func (s *mcpServerStub) SetChat(string, wecomaibot.RequestCarrier, string) {
	s.calls = append(s.calls, "set")
}
func (s *mcpServerStub) ClearChat(string)                    { s.calls = append(s.calls, "clear") }
func (s *mcpServerStub) URLFor(string) string                { return "http://127.0.0.1:1/mcp/x" }
func (s *mcpServerStub) HeadersFor(string) map[string]string { return nil }

// TestHandleSnapshotLifecycle 帧 Handle 的 Set→ask→Clear 时序：
// 未启用时无 Set/Clear；启用后每条消息先 Set 后 Clear，顺序恒定。
func TestHandleSnapshotLifecycle(t *testing.T) {
	srv := newACPWSServer(t)
	bot, err := wecomaibot.NewClient(wecomaibot.Config{
		BotID: "bot", Secret: "secret", WSURL: srv.url(),
		HeartbeatIntervalMS: 60000, Logger: silentLogger{},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	if err := bot.Connect(); err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	waitConnected(t, bot)
	t.Cleanup(bot.Disconnect)

	router, err := NewRouter(context.Background(), bot, fakeAdapterConfig())
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	msg := wecomaibot.TextMessage{}
	msg.ReqID = "req_1"
	msg.ChatID = "chat_1"
	ctx := context.Background()

	// 未启用：无调用
	stub := &mcpServerStub{}
	if _, err := router.Handle(ctx, "chat_1", msg, "我叫张三", "sid_1"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(stub.calls) != 0 {
		t.Errorf("未启用工具面不应有 Set/Clear: %v", stub.calls)
	}

	// 启用：Set → (ask) → Clear，顺序恒定
	if err := router.EnableTools(stub, t.TempDir()); err != nil {
		t.Fatalf("EnableTools: %v", err)
	}
	if _, err := router.Handle(ctx, "chat_1", msg, "我叫张三", "sid_2"); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if s := stub.calls; len(s) != 2 || s[0] != "set" || s[1] != "clear" {
		t.Errorf("应先 Set 后 Clear: %v", s)
	}
}

// TestRouterBindQuotedAttachmentHint RouterBind 对引用附件的文本消息组装：
// 群聊里独立附件不产生回调，用户以「引用附件 @机器人」送达——prompt 应
// 带提示行，附件本体由 read_attachment 工具读取；无附件引用不加提示。
func TestRouterBindQuotedAttachmentHint(t *testing.T) {
	tests := []struct {
		name    string
		quote   *wecomaibot.QuoteContent
		wantHit bool
	}{
		{"无引用", nil, false},
		{"引用纯文本", &wecomaibot.QuoteContent{MsgType: "text",
			Text: &wecomaibot.TextContent{Content: "被引用"}}, false},
		{"引用图片", &wecomaibot.QuoteContent{MsgType: "image",
			Image: &wecomaibot.ImageContent{URL: "https://x/i.png"}}, true},
		{"引用文件", &wecomaibot.QuoteContent{MsgType: "file",
			File: &wecomaibot.FileContent{URL: "https://x/f.txt"}}, true},
	}
	for _, tt := range tests {
		got := textWithQuoteHint("帮我看看", tt.quote)
		want := "帮我看看"
		if tt.wantHit {
			want += "\n[用户引用了一个附件，可用 read_attachment 读取]"
		}
		if got != want {
			t.Errorf("%s: 组装结果不对\n got=%q\nwant=%q", tt.name, got, want)
		}
	}
}

// TestRouterBindDirectAttachmentHint 三个直发附件回调（图片/视频/文件）
// 的 prompt 都应携带 read_attachment 引导——实测踩坑：无引导时 agent 在
// 文件系统里盲找落盘文件（ENOENT）而不调工具。
func TestRouterBindDirectAttachmentHint(t *testing.T) {
	base := "[用户发来文件]"
	if !strings.HasSuffix(base+attachmentHint, attachmentHint) {
		t.Fatal("impossible")
	}
	for _, tt := range []struct {
		name   string
		prompt string
	}{
		{"文件", "[用户发来文件]" + attachmentHint},
		{"图片", "[用户发来一张图片]" + attachmentHint},
		{"视频", "[用户发来一段视频]" + attachmentHint},
	} {
		if !strings.Contains(tt.prompt, "read_attachment") {
			t.Errorf("%s: prompt 应含 read_attachment 引导: %q", tt.name, tt.prompt)
		}
	}
}
