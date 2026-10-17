// acp-bot 是企业微信智能机器人对接外部编码代理（ACP agent）的完整示例：
// 企微每条推送消息成为一次发往持久 ACP 会话的 session/prompt，agent 的
// agent_message_chunk 流实时映射为企微流式回复（打字机效果）。
//
// 与 examples/llm-bot 的区别：llm-bot 把 LLM 直连在连接层里（内置轻量
// 模式），本示例把 agent 外置——连接层只做会话面桥接（ACP），
// 模型 providers、工具、权限全部由外部 agent（pi / Claude Code 等，
// 经 pi-acp / claude-agent-acp 适配器）自治。agent 会话跨企微消息持久，
// 同一企微会话天然获得对话记忆。
//
// 注意：无人值守场景下 agent 的权限请求默认自动拒绝（安全缺省）。
// agent 请求读文件等 fs 能力时同样拒绝——连接层不给 agent 暴露本机
// 文件系统。若你的 agent 需要 bash/edit 等能力，请在 agent 侧配置其
// 自身的授权策略，或设置 ACP_ALLOW_TOOL=1 启用本示例的宽松策略
// （全部 allow_once，仅用于可信 agent 的演示）。
//
// 环境变量：
//
//	WECOM_BOT_ID    机器人 ID（必填）
//	WECOM_BOT_SECRET 机器人 Secret（必填）
//	ACP_COMMAND     ACP 适配器可执行文件（必填），如 "pi-acp"
//	ACP_ARGS        适配器参数（可选，空格分隔）
//	ACP_ALLOW_TOOL  置 1 时对 agent 的权限请求全部允许（可选，默认全部拒绝）
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	wecomaibot "github.com/zhong/wecom-aibot-go"
	"github.com/zhong/wecom-aibot-go/acp"
)

func main() {
	botID := os.Getenv("WECOM_BOT_ID")
	secret := os.Getenv("WECOM_BOT_SECRET")
	command := os.Getenv("ACP_COMMAND")
	if botID == "" || secret == "" || command == "" {
		log.Fatal("需要 WECOM_BOT_ID / WECOM_BOT_SECRET / ACP_COMMAND")
	}
	cfg := acp.AgentConfig{
		Command:         command,
		Args:            strings.Fields(os.Getenv("ACP_ARGS")),
		PromptTimeoutMS: acp.DefaultPromptTimeoutMS,
	}
	if os.Getenv("ACP_ALLOW_TOOL") == "1" {
		cfg.Permission = func(context.Context, string, []string) bool { return true }
	}

	bot, err := wecomaibot.NewClient(wecomaibot.Config{
		BotID:                  botID,
		Secret:                 secret,
		MaxReconnectAttempts:   -1,
		MaxAuthFailureAttempts: -1,
		ReconnectIntervalMS:    1000,
		HeartbeatIntervalMS:    30000,
	})
	if err != nil {
		log.Fatalf("创建客户端失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	router, err := acp.NewRouter(ctx, bot, cfg)
	if err != nil {
		log.Fatalf("启动 ACP 适配器失败: %v（检查 ACP_COMMAND 与 agent 配置）", err)
	}
	acp.RouterBind(bot, router)
	log.Printf("wecom ⇄ ACP 桥已启动（适配器 %s）", command)

	if err := bot.Connect(); err != nil {
		log.Fatalf("连接失败: %v", err)
	}
	<-ctx.Done()
	bot.Disconnect()
}
