# 沙箱化 pi-agent：连接层集成草图

> 承接 [agent-sandbox-research.md](agent-sandbox-research.md)（选型：首选 Docker/OrbStack；P1 已在
> 本机验证容器内 pi-acp 全链路真实 LLM 调用)。本文回答：**怎么把它接进本仓库**，目标是
> `examples/acp-bot` 加一个环境开关即可跑在「pi 关进容器」的模式下。

## 边界重申：一个容器，不是每会话一个

`acp.Router` 把**全部**企微会话路由到**一个** `agentProc`（单个 pi-acp 子进程承载多个
ACP 会话）。所以沙箱边界是「主机 vs agent 进程树」——一个常驻容器即可；每会话隔离
（cwd 目录、专属 MCP 端点、Bearer）继续由既有机制在容器内承担，沙箱与之正交：

两个后端，同一镜像（OCI 通用），AgentConfig 只换 argv：

| | Docker（OrbStack） | Apple container（次强隔离路线，已实机验证） |
|---|---|---|
| argv 前缀 | `docker run --rm -i …` | `container run --rm -i …` |
| 计算边界 | Linux VM 内共享内核容器 | **每容器独立 Linux microVM（kata 内核）** |
| 容器→宿主 | `host.docker.internal`（引擎自动桥接全部回环端口） | 网关 IP `192.168.64.1`（稳定，实测）；服务 bind 网关或 bot 内置转发（见统一暴露面设计） |
| 中文面 | 生态更广 | Apple 官方维护，macOS 26+ |

### 容器→宿主：统一暴露面设计（v2，取代"宿主 TCP 转发器"小节）

**目标不变量：容器内 agent 看到的所有宿主服务都在同一个主机地址上**，即
`http://<ACP_SANDBOX_HOST>:<原端口>`——端口不变，只有主机名一个变量：

| | docker / OrbStack | Apple container |
|---|---|---|
| `ACP_SANDBOX_HOST` | `host.docker.internal`（引擎自动桥接 loopback） | `192.168.64.1`（vmnet 网关，宿主侧网桥接口 IP，多次 run 实测稳定） |
| mcp 工具面（仓库管的服务） | 监听 `127.0.0.1` + `SetAdvertiseBase`（已实现） | `ACP_TOOL_ADDR` 直接监听网关（已实现） |
| magpie 等外部回环服务 | **零机制**——`host.docker.internal` 天生可达 `127.0.0.1` | 二选一（见下）：宿主侧 bind 网关，或 acp-bot 内置转发 |

关键点：两条后端的差异被收敛到「mcp 监听在哪」这一处（Apple 能绑网关就直连，
docker 的 magic name 只在容器内可解析、宿主不能监听它，所以回环+通告），而
**容器内配置完全同形**：

```
pi models.json:  baseUrl = http://<ACP_SANDBOX_HOST>:3425/v1      （两后端一字不差，只换主机名）
每会话 mcp.json: url     = http://<ACP_SANDBOX_HOST>:<port>/mcp/<key>
```

Apple container 的官方 host 访问路径（`sudo container system dns create --localhost`）
有 sudo + pf + 禁用 iCloud Private Relay + 重启丢失四重副作用，弃用。网关直连无
任何系统级改动。

#### Apple 路线下外部回环服务的两条归宿

- **归宿 A（推荐给 magpie 这类自己有配置面的服务）：直接 `bind 192.168.64.1`。**
  零新增进程、零跳数（性能与直连一致）。缺点：要改外部服务自己的监听配置——
  这本就是部署者的配置职责边界。
- **归宿 B（不动外部服务，已实现）：acp-bot 内置回环转发。** env
  `ACP_HOSTFWD=<port>:<loopbackPort>[,...]`（监听地址 `ACP_HOSTFWD_BIND`，默认网关
  `192.168.64.1`），acp-bot 启动时在进程内起 goroutine 把 `<网关IP>:<port> →
  127.0.0.1:<loopbackPort>` 逐端口转发。外部服务继续只听回环、配置零改。
  生命周期优势：转发器与 bot 同进程——不存在「转发器挂了 bot 还活着」的半瘫态；
  单一进程重启即全量恢复。见 examples/acp-bot/hostfwd.go。

两案下 mcp 工具面都不走转发（自己人直接监听网关），转发只为外部服务兜底。

> docker 路线完全不需要 ACP_HOSTFWD——`host.docker.internal` 引擎级桥接全部回环
> 端口。这个 env 是 Apple 路线专属的补齐件。

#### 残余注意

- Apple 网关 IP `192.168.64.1` 按 vmnet 网桥分配——网络重建后理论上可漂移；
  `container run --rm alpine ip route` 看默认网关即可校正，或宿主 `ifconfig bridge100`。
- 绑定网关 = 暴露面收敛在容器网桥上，对外网不可见（不是 0.0.0.0 广播）；
  mcp 端点另有 Bearer 提权，magpie 的鉴权归 magpie 自己。

> 实测记录（2026-10-17）：`container build` 吃同一 Dockerfile（+`COPY --chown` 需显式
> `USER root` 段）；`container run -i` 裸 stdio 透传；e2e 全链路 initialize →
> session/new → prompt → 真实 LLM 流回「收到」；容器内非 root、`/etc` 拒写、宿主路径不可见。
> 转发器挂掉 = 容器失联但连接层报错文案照常兜底；转发器重启即恢复。

```
主机                                     容器（pi-agent:26，非 root）
┌────────────────────────────┐          ┌─────────────────────────────┐
│ examples/acp-bot           │  stdio    │ pi-acp                      │
│  ├─ Router(单 agentProc) ──┼──────────►│  └─ pi --mode rpc (多会话)  │
│  └─ mcp.WeComServer        │◄──────────┤   cwd=<workDir>/<key>       │
│     127.0.0.1:<port>       │  HTTP     │   经 host.docker.internal    │
└────────────────────────────┘           │   访问工具面 + LLM 代理      │
        ▲  企微 WS                        └─────────────────────────────┘
```

## 集成面清单

`acp.AgentConfig{Command, Args, Env, Cwd}` 透传 stdio，天然支持把 docker 当作「适配器」。
评估后**只有一个地方需要改代码**，其余是配置与镜像制造：

### 1. `ACP_COMMAND=docker`（零代码）

```bash
# AgentConfig 直接吃下（ACP_ARGS 空格分隔）：
ACP_COMMAND=docker
ACP_ARGS="run --rm -i \
  --name wecom-pi-agent \
  --cap-drop ALL --pids-limit 256 --memory 2g --cpus 2 \
  --network bridge \
  --user $(id -u):$(id -g) \
  --read-only --tmpfs /tmp \
  -v $HOME/.wecom-aibot/acp-work:$HOME/.wecom-aibot/acp-work \
  -e PI_CODING_AGENT_DIR=$HOME/.wecom-aibot/acp-work/.pi-agent \
  -e HOSTNAME_DOMAIN=host.docker.internal \
  pi-agent:26 pi-acp"
```

三个要点：

- **同径挂载**：workDir 以宿主绝对路径原样挂进容器。`enableSessionTools` 写的
  `.pi/mcp.json`、session/new 的 cwd、`read_attachment` 附件落盘（saveDir）全是
  宿主路径——同径挂载后容器内一字不改地成立，**免掉所有路径重写**（devcontainer 同款手法）。
  workDir 是 agent 惟一可写区，牺牲它换来：注入驱动的任意写入最多污染每会话工作目录，
  主机其余部分不可见。
- **`--user $(id -u):$(id -g)`**:容器进程与宿主同 uid ⇒ 挂载目录属主天然对齐（P1 验证时
  uid 501/100 错位 chmod 不救的坑，从根上消除），workDir 内容宿主可直接清理。
- **`--read-only` rootfs**：pi 的可写需求全部收窄到挂载的 workDir 与 tmpfs；镜像
  Home 无需再写（见下条）。

### 2. pi 配置搬家：`PI_CODING_AGENT_DIR`（零代码，替代 P1 的「烤进镜像」）

P1 验证时把 models.json/settings.json/trust.json 烤进镜像——每次改配置要重 build。生产化后
更顺的路线是**整个 pi agent 目录移进 workDir**：pi 尊重 `PI_CODING_AGENT_DIR` 环境变量，
把它指到 `<workDir>/.pi-agent`，则——

- `mergePiTrust` 写的 trust.json 落在挂载目录内（连接层零改动，`piAgentDir()` 已支持该 env 覆盖）；
- models.json / settings.json / auth.json 也在挂载目录里就地编辑（baseUrl 写
  `http://host.docker.internal:3425`）；
- 镜像变成**无状态通用件**（node26 + pi + pi-acp，非 root），配置与宿主解耦。

注意：pi-acp 自身要写的 `~/.pi/pi-acp` 仍需 HOME；`--user` 覆盖后 HOME 指向原属主目录
会不可写，启动包装加 `-e HOME=<workDir>/.pi-agent/home --workdir` 落在挂载区即可解决
（实施时验证一次）。

### 3. MCP 工具面通告地址（**唯一连接层代码改动**，~15 行）

`mcp.WeComServer.URL()` 硬编码回 `http://127.0.0.1:<port>` 写进每会话 `.pi/mcp.json`。
容器内的 pi 拿到它只会打到**容器自己的** loopback——够不到宿主工具面。需要加一个
通告基址覆盖：

```go
// mcp/server.go
// SetAdvertiseBase 设定对外通告的基址（默认空 = 用实际监听地址）。
// 宿主服务 + 容器内 agent 的部署里，通告 host.docker.internal / 网关 IP 才能被容器访问。
func (s *WeComServer) SetAdvertiseBase(base string) // URL() 前缀改用 base

// examples/acp-bot
// ACP_MCP_ADVERTISE_URL=http://host.docker.internal （docker；启用沙箱时必设）
// ACP_MCP_ADVERTISE_URL=http://192.168.64.1      （apple container；网关转发器后端口不变）
```

`Start` 仍监听 `127.0.0.1:0`（OrbStack 把 `host.docker.internal` 转发到宿主 loopback，
P1 已实测），Blessed token 不变。

## 生命周期与故障面

| 事件 | 现象 | 处置 |
|---|---|---|
| 容器启动失败/镜像缺失 | `NewRouter` → `initialize` 失败，`acp-bot` 启动报错退出 | 启动前检查 `docker image inspect pi-agent:26`，报错文案指明 build 步骤 |
| 长稳重启 | host 进程重启 ⇒ agentProc 关闭 ⇒ `--rm` 自动清理容器，重新拉起 | 无需额外维护 |
| 进程内 panic/agent 崩 | agotProc 读写 EOF ⇒ Handle 返回错误文案（现有兜底照旧） | 容器仍 `--rm` 干净退出，下轮重启；`--restart` 不需要（每次 NewRouter 一个新容器） |
| 镜像升级 | `docker build` 新 tag + 更新 ACP_ARGS | 建议示例 README:先 `docker stop` 再换 |

## 残余风险（沙箱管不住的，明确列出）

1. **网络出口无白名单**：默认 bridge 网络出站不设防（pi 调 LLM、MCP 需要出口）。
   域名级收紧需要本地出口 proxy（设置 `HTTPS_PROXY` 或 DNS 劫持方案），列为增强项而非首版。
2. **workDir 内任意写**：注入可让 agent 在工作区删除/改写会话文件。属可接受面
   （会话目录是 disposable），后续可加磁盘配额（`--storage-opt` 精细化）。
3. **MCP 工具面**：`send_template_card/send_markdown` 仍可被注入滥用向群内发消息——
   token 已按会话 pin 死端点，发送频控已实现（`mcp.SetSendMinInterval`，每会话
   最小间隔限速，examples/acp-bot 缺省启用，`ACP_SEND_MIN_INTERVAL_MS` 可调/关闭）；
   被限调用返回 ok:false 等待文案，模型可自纠。
4. **docker CLI 本身是提权面**：acp-bot 进程能跑 `docker run` 即能逃逸沙箱——这是运维
   面约束（宿主账号本身可信；不给远程用户触达 acp-bot 的面的路径），不在消息通道内。

## 镜像制造（收拢 P1 验证的产物）

`scripts/sandbox/Dockerfile`（把 P1 的临时验证镜像固化进仓库）：

```dockerfile
FROM node:26-alpine
RUN apk add --no-cache git ca-certificates && adduser -D pi
RUN npm install -g @earendil-works/pi-coding-agent@1.3.0 pi-acp@0.0.34
USER pi
```

配置一分不入镜像（§2 搬家）；构建脚本 `scripts/sandbox/build.sh` 一条 `docker build -t pi-agent:26 .`。
（P1 踩坑备忘：node 必须 26——18 崩 `crypto is not defined`；pi-acp 需要可写用户目录。）

## 实施切分（后续 todo 建议）

1. **T1 `mcp` 通告基址**：`SetAdvertiseBase` + 单测；`examples/acp-bot` 接 `ACP_MCP_ADVERTISE_URL`。工作量最小，独立可测，先行。
2. **T2 沙箱示例**：`scripts/sandbox/`（Dockerfile/build.sh/示例 env 文件）+ `examples/acp-bot` 文档注释补 `ACP_*_SANDBOX` 说明；不给 acp 包加任何沙箱代码——沙箱形态就是「换 argv」，这正是 AgentConfig 契约的设计回报。
3. **T3 端到端实测**：本机 OrbStack 起完整链（企微 WS → acp-bot → 容器内 pi → 工具面卡片），验证 §1 三个要点 + §2 HOME/PI_CODING_AGENT_DIR 落位；`--read-only` 下 pi 正常工作。
4. **T4**：~~MCP 发送频控~~（已实现，见上）；出口 proxy 域名白名单仍为增强项。

以上每步不破坏既有部署：不设 `ACP_MCP_ADVERTISE_URL`、`ACP_COMMAND=pi-acp` 直接跑时
行为与今日完全一致（沙箱是纯叠加，不改性）。
