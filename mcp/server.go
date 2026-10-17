package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	wecomaibot "github.com/zhong/wecom-aibot-go"
	"github.com/zhong/wecom-aibot-go/llm"
)

// DefaultAddr 是默认监听地址：仅回环、随机端口。工具端点只应被同机的
// agent（pi 等）访问，不要监听对外地址。
const DefaultAddr = "127.0.0.1:0"

// WeComServer 是进程内的企微工具面 MCP 服务端：工具目录取自
// llm.WeComToolDefs()，执行经 llm.NewWeComExecutor 绑定到 chatKey
// 对应的在途消息快照。
type WeComServer struct {
	bot    *wecomaibot.Client
	token  string
	reg    *registry
	tools  []mcpTool
	server *http.Server
	ln     net.Listener
}

// ToolSurface 是 acp 包集成所需的最小面（acp 单向依赖 mcp，不 import llm）。
type ToolSurface interface {
	// SetChat 在一条消息的 prompt 在途期间登记它的消息载体快照；
	// saveDir 是该会话 agent 的 cwd，附件落盘（read_attachment save=true）
	// 写到那里让 agent 可读，可为空。
	SetChat(chatKey string, req wecomaibot.RequestCarrier, saveDir string)
	// ClearChat 在 prompt 收尾后清除快照。
	ClearChat(chatKey string)
	// URLFor 返回该会话专属的 MCP 端点（写 agent 项目配置用）。
	URLFor(chatKey string) string
	// HeadersFor 返回该会话端点的请求头（写 agent 项目配置用，含 Bearer 提权）。
	HeadersFor(chatKey string) map[string]string
}

// NewWeComServer 创建绑定给定机器人的工具面服务端。token 为空时自动
// 生成（回显在 mcp.json 的 Authorization 头里）；addr 用 Start 传入。
func NewWeComServer(bot *wecomaibot.Client, token string) (*WeComServer, error) {
	if bot == nil {
		return nil, fmt.Errorf("mcp.NewWeComServer: bot 不能为 nil")
	}
	if token == "" {
		token = wecomaibot.GenerateRandomString(32)
	}
	s := &WeComServer{bot: bot, token: token, reg: newRegistry()}
	for _, def := range llm.WeComToolDefs() {
		s.tools = append(s.tools, mcpTool{
			Name:        def.Name,
			Description: def.Description,
			InputSchema: def.Parameters,
		})
	}
	return s, nil
}

// Start 在 addr 上监听并开始服务（后台运行），监听就绪即返回——
// 返回后 Addr/URL/URLFor 即可用。推荐 DefaultAddr（仅回环随机端口）。
// 服务错误经 Close 关停后不可恢复，常规部署下无需感知。
func (s *WeComServer) Start(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mcp 监听 %s 失败: %w", addr, err)
	}
	s.ln = ln
	s.server = &http.Server{Handler: s}
	go func() { _ = s.server.Serve(ln) }()
	return nil
}

// Addr 返回实际监听地址（如 "127.0.0.1:54321"）；未 Start 时为空。
func (s *WeComServer) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// URL 返回服务端基址（如 "http://127.0.0.1:54321"）。
func (s *WeComServer) URL() string {
	if s.Addr() == "" {
		return ""
	}
	return "http://" + s.Addr()
}

// URLFor 返回 chatKey 专属的 MCP 端点。chatKey 经 PathEscape 编码进
// 路径（dm: 开头的单聊键含冒号）。
func (s *WeComServer) URLFor(chatKey string) string {
	return s.URL() + "/mcp/" + chatKeyEscape(chatKey)
}

// HeadersFor 返回该会话端点的请求头：pi 的 mcp.json headers 惟此一项
// （Bearer 提权），Claude Code 等其他 agent 客户端同样兼容。
func (s *WeComServer) HeadersFor(chatKey string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + s.token}
}

// Token 返回提权 token（写 agent 配置 headers 用）。
func (s *WeComServer) Token() string { return s.token }

// Close 停止服务端。
func (s *WeComServer) Close() error {
	if s.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

// SetChat 实现 ToolSurface：登记在途消息快照。
func (s *WeComServer) SetChat(chatKey string, req wecomaibot.RequestCarrier, saveDir string) {
	s.reg.set(chatKey, req, saveDir)
}

// ClearChat 实现 ToolSurface：清除快照。
func (s *WeComServer) ClearChat(chatKey string) {
	s.reg.clear(chatKey)
}

// ServeHTTP 路由：Bearer 鉴权 → /mcp/<chatKey> 的 POST JSON-RPC；
// GET 一律 405（pi-mcp 视为无服务端流，正常降级）。
func (s *WeComServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(auth), []byte("Bearer "+s.token)) != 1 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"unauthorized"}}`))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	chatKey, err := chatKeyUnescape(rest)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveRPC(w, r, chatKey)
}

// serveRPC 处理一个 chatKey 端点上的 JSON-RPC 请求。
func (s *WeComServer) serveRPC(w http.ResponseWriter, r *http.Request, chatKey string) {
	// 请求体解析成 raw map：id 需原样回传（数字/字符串均可能）
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 10<<20))
	if err := dec.Decode(&raw); err != nil {
		writeJSONRPCError(w, nil, -32700, "解析请求失败: "+err.Error())
		return
	}
	id, hasID := raw["id"]
	methodRaw, _ := raw["method"]
	var method string
	_ = json.Unmarshal(methodRaw, &method)

	// 通知（method 有、id 无）：按 MCP 语义应答 202，无需回体
	if !hasID && method != "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var params map[string]json.RawMessage
	if p, ok := raw["params"]; ok {
		_ = json.Unmarshal(p, &params)
	}

	var result any
	var rpcErr *rpcError
	switch method {
	case "initialize":
		result = s.handleInitialize(params)
	case "tools/list":
		result = map[string]any{"tools": s.tools}
	case "tools/call":
		result, rpcErr = s.handleToolsCall(r.Context(), chatKey, params)
	case "ping":
		result = map[string]any{}
	default:
		rpcErr = &rpcError{code: -32601, message: "Method not found: " + method}
	}

	w.Header().Set("Content-Type", "application/json")
	if rpcErr != nil {
		writeJSONRPCErrorRaw(w, id, rpcErr.code, rpcErr.message)
		return
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result}
	_ = json.NewEncoder(w).Encode(resp)
}

// supportedVersions 与 echoProtocolVersion：客户端请求的版本在表内则原样
// 回显，否则回最新（pi-mcp 要求服务端版本必须在其支持表内）。
var supportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func echoProtocolVersion(requested string) string {
	for _, v := range supportedVersions {
		if v == requested {
			return v
		}
	}
	return supportedVersions[0]
}

// handleInitialize 应答 MCP 握手：回显协议版本，声明 tools 能力。
func (s *WeComServer) handleInitialize(params map[string]json.RawMessage) any {
	requested := ""
	if p, ok := params["protocolVersion"]; ok {
		_ = json.Unmarshal(p, &requested)
	}
	return map[string]any{
		"protocolVersion": echoProtocolVersion(requested),
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "wecom-aibot-go", "version": "0.1.0"},
	}
}

// handleToolsCall 执行一次工具调用：按 chatKey 定位在途消息快照，
// 构造 llm.NewWeComExecutor 执行。快照未命中（该会话没有进行中的
// prompt）返回 ok:false 的正常工具结果，模型可自纠。
func (s *WeComServer) handleToolsCall(ctx context.Context, chatKey string, params map[string]json.RawMessage) (any, *rpcError) {
	name := ""
	if p, ok := params["name"]; ok {
		_ = json.Unmarshal(p, &name)
	}
	known := false
	for _, t := range s.tools {
		if t.Name == name {
			known = true
			break
		}
	}
	if !known {
		return nil, &rpcError{code: -32602, message: "Unknown tool: " + name}
	}
	var args json.RawMessage = json.RawMessage("{}")
	if p, ok := params["arguments"]; ok && len(p) > 0 {
		args = p
	}

	entry, ok := s.reg.get(chatKey)
	if !ok {
		return toolTextResult(`{"ok":false,"errcode":0,"errmsg":"该会话当前没有进行中的消息，无法执行企微工具"}`), nil
	}
	exec := llm.NewWeComExecutorCwd(s.bot, entry.req, entry.saveDir)
	out, err := exec(ctx, name, args)
	if err != nil {
		return nil, &rpcError{code: -32603, message: err.Error()}
	}
	return toolTextResult(out), nil
}

// toolTextResult 包一个文本 content 的 MCP 工具结果。
func toolTextResult(text string) any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": false,
	}
}

// rpcError 是 JSON-RPC 层错误（区别于工具执行的 ok:false 结果）。
type rpcError struct {
	code    int
	message string
}

// mcpTool 是 MCP 线格式的工具描述（llm.ToolDef 的 parameters → inputSchema）。
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// writeJSONRPCError 写一个 id 为 null 的 JSON-RPC 错误（请求不可解析时）。
func writeJSONRPCError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}

// writeJSONRPCErrorRaw 写一个回传原始 id（json.RawMessage）的 JSON-RPC 错误。
func writeJSONRPCErrorRaw(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	})
}
