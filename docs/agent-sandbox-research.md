# ACP Agent 沙箱方案调研

> 场景：企微智能机器人面向企业内**所有人**开放，pi 作为 agent 经本仓库 acp 包接入。
> 威胁模型：任意企微用户可通过消息诱导 agent 执行任意 prompt（含注入攻击），
> pi 无内置沙箱、以启动账号全权限运行（[官方安全文档](https://pi.dev/docs/latest/security)
> 明确建议"整个 agent 放进容器/VM"）。必须假设 pi 进程（及其 spawn 的 shell、工具）会被
> 任意指令驱动——沙箱边界就是唯一的防线。

## 结论（TL;DR）

| 优先级 | 方案 | 一句话 |
|---|---|---|
| **首选（已双验证）** | **Apple `container`（brew 1.5.0）** | 每容器独立 Linux microVM（强于共享内核容器），本机已全链路实测；OCI 镜像与 docker 通用，argv 换成 `container run -i` 即接入 |
| 平滑回退（同为已验证路径） | **Docker（本机 OrbStack 后端）** | 机器上已经在跑，容器内跑 pi-acp，`acp.AgentConfig` 零改动即可指挥；与 Apple container 共用同一镜像 |
| 观察（agent 原生生态） | **microsandbox** | 专为 AI agent 设计的 microVM（Hypervisor.framework），MCP 管沙箱生命周期，但 beta |
| 纵深防御补充 | **pi-sandbox 扩展** | pi 原生工具层限制，可与容器叠加；不建议单独承重 |
| ❌ 排除 | gVisor / Firecracker / E2B Embed | 均需 Linux+KVM 宿主，macOS 上不可用；E2B 云为付费托管 |
| ❌ 排除 | macOS `sandbox-exec` 手写 profile | 已被 Apple 弃用（无替代时间表），且无法做域名级网络过滤，需叠 proxy |

## 对比矩阵

| 维度 | Docker/OrbStack | Apple container | microsandbox | pi-sandbox 扩展 |
|---|---|---|---|---|
| 隔离原理 | Linux VM + 共享内核容器 | **每容器一个轻量 Linux VM** | 每沙箱一个 microVM | 进程级 Seatbelt（macOS）/bubblewrap（Linux） |
| 隔离强度 | 中高（容器逃逸面=内核漏洞） | 高（独立 guest 内核） | 高（独立 guest 内核） | 中（用户态；**sudo 批准后出沙箱**） |
| 本机可用性 | ✅ **已安装且在运行** | ✅ macOS 26+（本机 26.3.1），brew 可装 | ✅ Apple Silicon beta | ✅ `pi install npm:pi-sandbox` |
| 启动开销 | 秒级（镜像常驻） | 亚秒（WWDC26：sub-second） | <100ms（M1 实测口径） | 无（进程内） |
| pi 适配 | 镜像内装 node/pi/pi-acp | 同左（OCI 镜像通用） | OCI 镜像通用 | pi 原生 |
| 无人值守 | ✅ | ✅ | ✅（SDK/CLI/MCP） | ✅（.pi/sandbox.json 配置文件化） |
| 网络控制 | 网络模型+可选断网（域级过滤需 proxy） | 同左 | 有挂载/网络控制面 | 表达有限（IP/端口，无域名级） |
| iOS/维护风险 | 低（Docker 事实标准） | 中（Apple 新项目，快速迭代） | 中（beta，社区项目） | 中（单人维护扩展） |
| 与 AgentConfig 契合 | `Command:"docker", Args:[run …]` | `Command:"container"` | 需走其 CLI/SDK | 无需改动（pi 侧自我约束） |

## 与本仓库的集成契约（关键：acp 包已经为此做好了准备）

`acp.AgentConfig{Command, Args, Env, Cwd}` + `agentProc` 用 stdin/stdout JSON-RPC 对话，
意味着**任何「接收 argv、透传 stdio」的包装器都能直接替换 pi-acp**，连接层零改动：

```
AgentConfig{
  Command: "docker",
  Args: []string{
    "run", "--rm", "-i",                      // -i 保留 stdin，ACP stdio 透传
    "--network", "pi-net",                    // 专用网络（见下）
    "-v", "<workDir>/<chatKey>:/work",        // 每会话 cwd 挂载（trust.json/.pi/mcp.json 照常生效）
    "-e", "LLM_API_KEY", "-e", "TEST_MCP_URL=http://host.docker.internal:<port>/mcp/<chatKey>",
    "pi-agent:latest", "pi-acp",
  },
}
```

- **MCP 工具面**：`mcp.WeComServer` 监听 `127.0.0.1`，容器内经 `host.docker.internal`（OrbStack
  支持）访问，Bearer token 不变。
- **网络策略**：pi 需要 LLM API 出口，不能断网。妥协有三档：①只做专用 bridge 网络隔离（默认）；
  ②再加出口代理做域名白名单（LLM API + host.docker.internal）；③`--network none` + 代理是唯一出口。
- **加固清单**：容器内非 root 用户、不挂 docker socket、只读 rootfs、内存/CPU 上限、
  `--cap-drop ALL`、镜像内不留宿主凭据。
- **残余风险（沙箱管不住的）**：工具面 `send_template_card/send_markdown` 可被注入滥用向群里
  发消息——token 只授予本会话端点 + 连接层可加发送频控；这个面在沙箱之外，值得后续单独收紧。

## 推荐路径

1. **P1 验证（已完成 2026-10-17，本机 OrbStack）**：
   - 镜像 `node:26-alpine`（必须 ≥ pi 所需的 node 26；node:18 下 pi-acp 崩 `crypto is not defined`）
     + `@earendil-works/pi-coding-agent@1.3.0` + `pi-acp@0.0.34`，运行用户 `pi`（非 root）。
   - pi 配置烤进镜像（`models.json` 的 baseUrl 改 `host.docker.internal:3425`，
     `settings.json` 沿用宿主的 `quietStartup: true`——startup info 坑正解）；容器内
     pi-acp 需要可写的 `~/.pi/pi-acp`，所以不能整目录 ro 挂载。
   - **结果**：initialize → session/new → session/prompt 全通，pi 经宿主 magpie 代理完成
     真实 LLM 调用并流回 chunk「收到」；FS 隔离验证：`/etc` 写入被拒、宿主路径不可见。
   - **OrbStack 结论**：容器经 `host.docker.internal` 可达宿主 loopback —— mcp 工具面
     (`127.0.0.1:<port>/mcp/<chatKey>`) 无需改监听地址即可被容器内 agent 访问。
3. **Apple container 可行性验证（已完成 2026-10-17，本机 brew 1.5.0 + macOS 26.3.1）**：
   - 安装即用：`brew install container` + `container system start --disable-kernel-install`
     + `container system kernel set --recommended`（首次拉 kata 内核 ~1 分钟）。
   - **同一套 P1 Dockerfile 零改动直接 `container build`**（BuildKit 后端）：node:26-alpine
     + pi + pi-acp 镜像构建成功，`COPY --chown=pi:pi` 需显式 `USER root` 段（与 docker 语义相同）。
   - **ACP stdio 全链路跑通**：宿主 node 脚本 spawn `container run --rm -i pi-agent-ac:full pi-acp`
     → initialize → session/new → session/prompt → pi 经网关转发器调宿主 magpie → 真实 LLM
     流回「收到」。`container run -i`（不带 -t）裸 stdio 透传无损，AgentConfig 契约成立。
   - **容器→宿主 loopback 的无 sudo 方案**（规避 pf/Private Relay 副作用）：容器默认网关
     `192.168.64.1` 稳定（跨多次 run 实测），宿主跑一个小 TCP 转发器（绑定网关 IP:端口 →
     127.0.0.1 目标，如 `192.168.64.1:13425 → 127.0.0.1:3425`）即可让容器访问 magpie 与
     mcp 工具面。对比官方路径 `sudo container system dns create host.container.internal
     --localhost …`（pf 规则，禁用 iCloud Private Relay、重启丢失），转发器方案不碰系统级配置。
     注：官方 0.9.0+ 虽支持 `host.docker.internal` DNS 名，但 pf 转发仍需 sudo 手动建。
   - **隔离验证**：容器内非 root（uid 100 pi）、`/etc` 写入被拒、宿主路径（/Users）不可见
     ——独立 microVM 边界成立（比共享内核容器强一档）。
   - 运维特性：每次 `container run` 亚秒级启动；同一镜像可被 docker 与 Apple container 双跑
     （OCI 兼容），迁移成本≈0。
4. 验证通过后按上契约把 `ACP_COMMAND` 换成 container 包装，`examples/acp-bot` 加一个
   `sandbox` 示例（可选 env 开关，后端 docker / apple container 二选一）。
5. 隔离要求升级时横跳 Apple `container`（brew 一装）——AgentConfig 契约不变，只换 argv
   （**已验证可行，见第 3 条**）。

## 附：排除项归因

- **gVisor**（Sentry 用户态内核）与 **Firecracker**（KVM microVM）：宿主必须是 Linux，
  macOS 上至多装在 VM 里——VM in VM 无意义，排除。
- **E2B**：云服务收费；其本地版 Embed 也要求 Linux+KVM，排除。
- **`sandbox-exec`**：Apple [containerization#737](https://github.com/apple/containerization/issues/737)
  确认其处于弃用状态且无官方替代时间表；Seatbelt 无法做域名级网络过滤（社区方案都要逼流量
  过本地 proxy 补洞）；无人值守下 profile 调试成本高。仅当不愿引入任何 VM 时才考虑。
- **pi-sandbox 单独承重**：README 自认浏览器示例有明显漏洞；Linux 下 write glob 对 Bash
  静默不生效；sudo 类命令批准后**绕过沙箱**（permission-modes 文档）——对"面向全员开放"
  的场景不可接受为唯一防线。

## 参考来源

- pi 安全模型（无内置沙箱，建议 VM）：https://pi.dev/docs/latest/security
- pi-sandbox 扩展：https://github.com/carderne/pi-sandbox
- Apple container / containerization：https://github.com/apple/container ·
  https://opensource.apple.com/projects/containerization/ ·
  WWDC26 container machines（亚秒级启动）：https://developer.apple.com/videos/play/wwdc2026/389/
- microsandbox：https://github.com/superradcompany/microsandbox · https://microsandbox.dev/
- sandbox-exec 弃用与 Agent 沙箱实践：https://github.com/apple/containerization/issues/737 ·
  https://github.com/michaelneale/agent-seatbelt-sandbox · https://github.com/danra/chopi
- 隔离技术对比（gVisor vs Firecracker）：https://gvisor.dev/docs/architecture_guide/intro/ ·
  https://github.com/firecracker-microvm/firecracker/blob/main/SPECIFICATION.md
