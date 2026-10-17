// Package acp 把外部编码代理（pi、Claude Code 等）经 ACP
// （Agent Client Protocol，JSON-RPC 2.0 over stdio）接入企业微信智能机器人。
//
// 定位：本仓库只做 agent 与企业微信之间的连接层。会话面走 ACP——
// 企微每条推送消息成为一次发往持久 agent 会话的 session/prompt，
// agent 的 agent_message_chunk 流映射为企微流式回复帧（打字机）；
// 工具面仍由 agent 侧自行接入（如 MCP），不经本包转发。
//
// 本包只依赖 Go 标准库与既有 gorilla/websocket（经根包），不引入
// ACP SDK；单向依赖根包（wecomaibot），根包不感知本包。
package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// DefaultPromptTimeoutMS 是一次 session/prompt（含 agent 全部工具轮）的
// 默认超时（毫秒），命名对齐根包 Config 的 XxxMS 约定。
const DefaultPromptTimeoutMS = 300000

// AgentConfig 描述一个 ACP 适配器子进程。Command 是适配器可执行文件
// （如 "pi-acp"），Args/Cwd/Env 透传给子进程。
type AgentConfig struct {
	Command string
	Args    []string
	Cwd     string
	Env     []string
	// PromptTimeoutMS 单次 prompt 的超时；<=0 用 DefaultPromptTimeoutMS。
	PromptTimeoutMS int
	// Permission 决定 session/request_permission 的自动应答；nil 表示
	// 一律拒绝（无人值守默认安全）。见 AllowFunc。
	Permission AllowFunc
}

// AllowFunc 对 agent 发来的权限请求给出应答：返回 true 表示选中
// allow_once（同意本次），false 表示 reject_once（拒绝本次）。
// 无人值守场景下由连接层按策略答复，见包注释。
type AllowFunc func(ctx context.Context, title string, optionNames []string) bool

// agentProc 驱动一个 ACP 适配器子进程：写入 JSON-RPC 请求、
// 读出响应与通知，按会话分发 update。
type agentProc struct {
	cfg AgentConfig

	writeMu sync.Mutex
	stdinW  io.Writer

	mu      sync.Mutex
	nextID  int
	pending map[int]chan map[string]any // 请求 id → 响应通道

	// onUpdate: sessionId → 处理器；onRequest: sessionId → 权限请求处理器
	handlersMu sync.RWMutex
	onUpdate   map[string]func(map[string]any)
}

func newAgentProc(ctx context.Context, cfg AgentConfig) (*agentProc, error) {
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}
	if len(cfg.Env) > 0 {
		cmd.Env = cfg.Env
	}
	cmd.Stderr = nil // 适配器日志直接丢弃（连接层不解析）
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 ACP 适配器 %s: %w", cfg.Command, err)
	}
	p := &agentProc{
		cfg:      cfg,
		stdinW:   stdin,
		pending:  map[int]chan map[string]any{},
		onUpdate: map[string]func(map[string]any){},
	}
	go p.readLoop(stdout)
	return p, nil
}

// readLoop 逐行读取 JSON-RPC：响应按 id 送回 pending 通道，通知与方法
// 调用（如 session/request_permission）走 dispatch。
func (p *agentProc) readLoop(stdout io.ReadCloser) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var raw map[string]any
		if json.Unmarshal(sc.Bytes(), &raw) != nil || raw == nil {
			continue
		}
		id, hasID := raw["id"]
		_, hasMethod := raw["method"]
		if hasID && !hasMethod {
			// 响应
			p.mu.Lock()
			ch := p.pending[int(id.(float64))]
			delete(p.pending, int(id.(float64)))
			p.mu.Unlock()
			if ch != nil {
				ch <- raw
			}
			continue
		}
		if hasMethod {
			p.dispatch(raw)
		}
	}
}

// dispatch 处理服务端通知与请求：session/update 按会话分发，
// session/request_permission 按 Permission 策略自动应答（无人值守：
// 无策略一律拒绝——agent 侧不得把未知结果当同意，拒绝是安全缺省）。
func (p *agentProc) dispatch(raw map[string]any) {
	method, _ := raw["method"].(string)
	switch method {
	case "session/update":
		params, _ := raw["params"].(map[string]any)
		sid, _ := params["sessionId"].(string)
		p.handlersMu.RLock()
		h := p.onUpdate[sid]
		p.handlersMu.RUnlock()
		if h != nil {
			// 同步调用：readLoop 单线程序列化 update 分发，保证
			// session/prompt 响应到达时，其前的 chunk 一定已被处理。
			// handler 只做累积与非阻塞发帧，无长阻塞操作。
			h(raw)
		}
	case "session/request_permission":
		p.answerPermission(raw)
	default:
		// 其余服务端请求（fs/read_text_file 等）：拒绝——无人值守连接层
		// 不提供文件系统能力。
		if _, hasID := raw["id"]; hasID {
			p.reply(raw, map[string]any{
				"outcome": map[string]any{"outcome": "cancelled"},
			})
		}
	}
}

// answerPermission 对权限请求自动应答：遍历 options，AllowFunc 同意则选
// 首个 allow_once/reject_once 决定；无策略或策略拒绝则 reject_once；
// 两类选项都没有则 cancelled。
func (p *agentProc) answerPermission(raw map[string]any) {
	params, _ := raw["params"].(map[string]any)
	options, _ := params["options"].([]any)
	title, _ := params["title"].(string)

	names := make([]string, 0, len(options))
	allowID, rejectID := "", ""
	for _, o := range options {
		m, ok := o.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["optionId"].(string)
		kind, _ := m["kind"].(string)
		name, _ := m["name"].(string)
		names = append(names, name)
		switch kind {
		case "allow_once", "allow_always":
			if allowID == "" {
				allowID = id
			}
		case "reject_once", "reject_always":
			if rejectID == "" {
				rejectID = id
			}
		}
	}

	outcome := map[string]any{"outcome": "cancelled"}
	switch {
	case allowID == "" && rejectID == "":
		// 无可选项：cancelled
	case p.cfg.Permission == nil:
		outcome = map[string]any{"outcome": "selected", "optionId": rejectID}
	case p.cfg.Permission != nil && p.cfg.Permission(context.Background(), title, names):
		outcome = map[string]any{"outcome": "selected", "optionId": allowID}
	default:
		outcome = map[string]any{"outcome": "selected", "optionId": rejectID}
	}
	p.reply(raw, map[string]any{"outcome": outcome})
}

// reply 回一个 JSON-RPC 响应（复用请求的 id）。
func (p *agentProc) reply(request map[string]any, result any) {
	id, _ := request["id"].(float64)
	idInt := int(id)
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      idInt,
		"result":  result,
	})
	if err != nil {
		return
	}
	p.writeMu.Lock()
	_, _ = p.stdinW.Write(append(b, '\n'))
	p.writeMu.Unlock()
}

// call 发请求并等响应；timeout 为 0 时不限时。
func (p *agentProc) call(ctx context.Context, method string, params any, timeoutMS int) (map[string]any, error) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan map[string]any, 1)
	p.pending[id] = ch
	p.mu.Unlock()

	b, err := json.Marshal(rpcRequest{Jsonrpc: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	p.writeMu.Lock()
	_, err = p.stdinW.Write(append(b, '\n'))
	p.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("ACP 写入失败: %w", err)
	}

	wait := (<-chan map[string]any)(ch)
	if timeoutMS > 0 {
		select {
		case r := <-wait:
			return r, nil
		case <-ctx.Done():
			p.mu.Lock()
			delete(p.pending, id)
			p.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	select {
	case r := <-wait:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// rpcRequest 是发送侧的 JSON-RPC 请求体。
type rpcRequest struct {
	Jsonrpc string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// registerUpdate 注册按会话分发 session/update 的处理器。
func (p *agentProc) registerUpdate(sessionID string, h func(map[string]any)) {
	p.handlersMu.Lock()
	p.onUpdate[sessionID] = h
	p.handlersMu.Unlock()
}

// initialize 完成握手，返回适配器声明的 agent 信息（用不上则忽略）。
func (p *agentProc) initialize(ctx context.Context) error {
	r, err := p.call(ctx, "initialize", map[string]any{
		"client":             map[string]any{"name": "wecom-aibot-go", "version": "0.1.0"},
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"read": false, "write": false}},
	}, 60000)
	if err != nil {
		return fmt.Errorf("ACP initialize: %w", err)
	}
	if r["error"] != nil {
		return fmt.Errorf("ACP initialize: %v", r["error"])
	}
	return nil
}

// newSessionOnly 创建一个会话（握手已完成），返回 sessionId。
func (p *agentProc) newSessionOnly(ctx context.Context) (string, error) {
	r, err := p.call(ctx, "session/new", map[string]any{"cwd": "/tmp", "mcpServers": []any{}}, 60000)
	if err != nil {
		return "", fmt.Errorf("ACP session/new: %w", err)
	}
	if r["error"] != nil {
		return "", fmt.Errorf("ACP session/new: %v", r["error"])
	}
	sid, _ := r["result"].(map[string]any)["sessionId"].(string)
	if sid == "" {
		return "", fmt.Errorf("ACP session/new 未返回 sessionId")
	}
	return sid, nil
}

// prompt 发送一条用户消息，返回 agent 的 stopReason。
func (p *agentProc) prompt(ctx context.Context, sessionID, text string) (string, error) {
	timeout := p.cfg.PromptTimeoutMS
	if timeout <= 0 {
		timeout = DefaultPromptTimeoutMS
	}
	r, err := p.call(ctx, "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "text", "text": text}},
	}, timeout)
	if err != nil {
		return "", err
	}
	if r["error"] != nil {
		return "", fmt.Errorf("ACP session/prompt: %v", r["error"])
	}
	stop, _ := r["result"].(map[string]any)["stopReason"].(string)
	return stop, nil
}
