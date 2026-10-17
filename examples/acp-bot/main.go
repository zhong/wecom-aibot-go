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
// 工具面（可选）：ACP_TOOLS=1 时，进程内启动 MCP 服务端（仅回环随机
// 端口 + Bearer 提权），每个企微会话独立一个 agent 会话目录
// （ACP_TOOL_WORKDIR 下），目录里的 .pi/mcp.json 指向该会话专属端点
// ——pi 经项目配置以 MCP 客户端身份接入，模型可自主调用
// send_template_card / send_markdown / read_attachment。pi对 MCP 工具
// 调用默认不弹权限（不受 ACP_ALLOW_TOOL / Permission 策略覆盖）。
// 启动时会自动把 workDir 根目录登记进 pi 的 trust.json（项目级
// .pi/mcp.json 仅在受信目录加载，RPC 模式无法弹窗询问）。
//
// 环境变量：
//
//	WECOM_BOT_ID     机器人 ID（必填）
//	WECOM_BOT_SECRET 机器人 Secret（必填）
//	ACP_COMMAND      ACP 适配器可执行文件（必填），如 "pi-acp"
//	ACP_ARGS         适配器参数（可选，空格分隔）
//	ACP_ALLOW_TOOL   置 1 时对 agent 的权限请求全部允许（可选，默认全部拒绝）
//	ACP_TOOLS        置 1 时启用企微工具面（可选）
//	ACP_TOOL_WORKDIR 会话 agent 目录基址（可选，默认 ~/.wecom-aibot/acp-work；
//	                 须跨重启稳定——trust 登记按路径生效）
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	wecomaibot "github.com/zhong/wecom-aibot-go"
	"github.com/zhong/wecom-aibot-go/acp"
	"github.com/zhong/wecom-aibot-go/mcp"
)

// defaultToolWorkDir 是 ACP_TOOL_WORKDIR 缺省时的会话目录基址。
func defaultToolWorkDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".wecom-aibot", "acp-work")
}

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

	// 工具面（可选）：进程内 MCP 服务端 + 每会话目录 + pi trust 登记。
	if os.Getenv("ACP_TOOLS") == "1" {
		workDir := os.Getenv("ACP_TOOL_WORKDIR")
		if workDir == "" {
			workDir = defaultToolWorkDir()
		}
		if workDir == "" {
			log.Fatal("ACP_TOOLS=1 需要可定位的 home 目录，或显式设置 ACP_TOOL_WORKDIR")
		}
		if err := mergePiTrust(workDir); err != nil {
			log.Fatalf("登记 pi 项目信任失败: %v", err)
		}
		mcpSrv, err := mcp.NewWeComServer(bot, "")
		if err != nil {
			log.Fatalf("启动工具面服务端失败: %v", err)
		}
		if err := mcpSrv.Start(mcp.DefaultAddr); err != nil {
			log.Fatalf("工具面服务端监听失败: %v", err)
		}
		defer mcpSrv.Close()
		if err := router.EnableTools(mcpSrv, workDir); err != nil {
			log.Fatalf("启用工具面失败: %v", err)
		}
		log.Printf("工具面已启用: %s（会话目录 %s）", mcpSrv.URLFor("chatid"), workDir)
	}

	acp.RouterBind(bot, router)
	log.Printf("wecom ⇄ ACP 桥已启动（适配器 %s）", command)

	if err := bot.Connect(); err != nil {
		log.Fatalf("连接失败: %v", err)
	}
	<-ctx.Done()
	bot.Disconnect()
}
