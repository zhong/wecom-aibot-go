// piconfig.go 管理 pi 的用户级配置登记：把本示例的会话 workDir 根目录
// 合并进 ~/.pi/agent/trust.json（项目 trust 决策）——pi 的项目级
// .pi/mcp.json 只在受信目录加载，而 RPC 模式（pi-acp 使用）无法弹窗
// 询问 trust，必须预先登记。只增不改：其他路径的既有决策一律保留。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// piAgentDir 返回 pi 的用户级 agent 目录（PI_CODING_AGENT_DIR 可覆盖）。
func piAgentDir() string {
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

// mergePiTrust 把 dir（绝对路径）登记为 pi 受信项目目录，写入
// <agentDir>/trust.json。兼并语义：读已有文件 → upsert 该路径为 true →
// 排序写回（对齐 pi 的 ProjectTrustStore 写序）；文件缺失则创建。
func mergePiTrust(dir string) error {
	agentDir := piAgentDir()
	if agentDir == "" {
		return fmt.Errorf("无法定位 pi agent 目录")
	}
	if !filepath.IsAbs(dir) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return fmt.Errorf("解析绝对路径失败: %w", err)
		}
		dir = abs
	}
	trustPath := filepath.Join(agentDir, "trust.json")

	data := map[string]bool{}
	if raw, err := os.ReadFile(trustPath); err == nil {
		if err := json.Unmarshal(raw, &data); err != nil {
			// 已有文件畸形：保守失败而不是覆盖（可能是 pi 正在写）
			return fmt.Errorf("解析 %s 失败（请人工确认）: %w", trustPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取 %s 失败: %w", trustPath, err)
	}
	data[dir] = true

	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", agentDir, err)
	}
	if err := os.WriteFile(trustPath, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", trustPath, err)
	}
	return nil
}
