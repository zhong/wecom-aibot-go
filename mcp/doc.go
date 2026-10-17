// Package mcp 在连接层进程内起一个仅监听回环地址的极简 MCP
// （Model Context Protocol，Streamable HTTP 传输）服务端，把企微工具面
// （模板卡片 / markdown / 读附件，即 llm.WeComToolDefs 的三个工具）暴露给
// 外部 agent（pi、Claude Code 等）。
//
// 为什么是 HTTP 服务端而不是 stdio 子进程：agent（如经 pi-acp 桥接的 pi）
// 自己就是 MCP 客户端，从其配置文件（~/.pi/agent/mcp.json 或项目 .pi/mcp.json）
// 拉起 stdio server；工具执行需要连接进程持有的 wecomaibot.Client 与当前
// 消息载体，HTTP 端点让工具面留在连接进程内。每个企微会话一个独立端点
// 路径（/mcp/<chatKey>），会话定位由 URL 完成——模型无法跨会话投放。
//
// 只依赖 Go 标准库（net/http）；依赖根包（wecomaibot）与 llm 包
// （工具目录与执行器），二者均不改。
package mcp
