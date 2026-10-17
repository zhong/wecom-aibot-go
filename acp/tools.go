package acp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/zhong/wecom-aibot-go/mcp"
)

// EnableTools 给 Router 启用企微工具面。surface 通常是 mcp.NewWeComServer
// 的返回值；workDir 是每个 agent 会话的专属 cwd 基目录——每个新会话在
// workDir/<chatKey>/.pi/mcp.json 写入指向会话专属 MCP 端点的配置
// （server "wecom"，exposure direct，模型直接可见），并以该目录作为
// session/new 的 cwd 传给 agent。agent 由此把工具调用固定在本会话的
// 端点上，无法跨会话投放。
//
// 必须在首条消息前调用（Connect/RouterBind 之前），未启用时 Router
// 行为与既有版本完全一致（cwd=/tmp、无快照登记）。
//
// 注意：workDir 必须由调用方保证跨进程重启稳定，且通常应配合 examples
// 的 trust 登记把 workDir 根目录标记为受信（pi 的项目级 .pi/mcp.json 仅在
// 受信目录加载；RPC 模式无法弹窗询问）。见 examples/acp-bot 的
// mergePiTrust。
func (r *Router) EnableTools(surface mcp.ToolSurface, workDir string) error {
	if surface == nil {
		return fmt.Errorf("acp.EnableTools: surface 不能为 nil")
	}
	if workDir == "" {
		return fmt.Errorf("acp.EnableTools: workDir 不能为空")
	}
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return fmt.Errorf("acp.EnableTools: 创建 workDir %s 失败: %w", workDir, err)
	}
	toolsStateMu.Lock()
	r.surface = surface
	r.toolWorkDir = workDir
	toolsStateMu.Unlock()
	return nil
}

// toolSurface 读当前工具面状态（nil 表示未启用）。
func (r *Router) toolSurface() (mcp.ToolSurface, string) {
	toolsStateMu.RLock()
	defer toolsStateMu.RUnlock()
	return r.surface, r.toolWorkDir
}

// toolsStateMu 保护 Router 上的工具面字段（EnableTools 写一次，Handle/entry 惰性读）。
var toolsStateMu sync.RWMutex

// safeKey 把 chatKey 编码为既可作 URL 路径段（与 mcp 端点编码一致）又可作
// 目录名的安全形式：URL 保留字符（: / ? # 等）与路径分隔符都会被转义。
func safeKey(chatKey string) string {
	return url.PathEscape(chatKey)
}

// sessionWorkDir 返回某个企微会话的专属 cwd 目录。
func sessionWorkDir(workDir, chatKey string) string {
	return filepath.Join(workDir, safeKey(chatKey))
}

// enableSessionTools 为一个新 agent 会话准备工具面目录：
// workDir/<key>/.pi/mcp.json，幂等覆盖（重启后 URL/token 变化时刷新）。
func (r *Router) enableSessionTools(chatKey string, surface mcp.ToolSurface, workDir string) error {
	dir := sessionWorkDir(workDir, chatKey)
	if err := os.MkdirAll(filepath.Join(dir, ".pi"), 0o700); err != nil {
		return fmt.Errorf("创建会话目录 %s 失败: %w", dir, err)
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"wecom": map[string]any{
				"url":      surface.URLFor(chatKey),
				"headers":  surface.HeadersFor(chatKey),
				"exposure": "direct",
			},
		},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ".pi", "mcp.json"), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入会话 mcp.json 失败: %w", err)
	}
	return nil
}
