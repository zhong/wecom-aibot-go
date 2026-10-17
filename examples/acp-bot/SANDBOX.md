# 沙箱化 pi-agent 运行示例（examples/acp-bot）

两个已实测的后端，同一镜像（`scripts/sandbox/build.sh` 构建）。核心接线差异只有两点：
**容器内 pi 配置的 LLM baseUrl** 和 **工具面监听/通告地址**。

## 公共准备

```bash
# 1. 构建镜像（docker 或 apple 二选一）
scripts/sandbox/build.sh apple          # 或: scripts/sandbox/build.sh docker

# 2. workDir（host 侧基址；容器同径挂载，路径在内外一致）
mkdir -p ~/.wecom-aibot/acp-work
```

## Apple container（更强隔离：每容器独立 Linux microVM）

工具面**直接监听容器网关** `192.168.64.1`（宿主侧的 vmnet 网桥 IP，容器按此访问
宿主），零转发器、零 sudo：

```bash
export WECOM_BOT_ID=...  WECOM_BOT_SECRET=...
export ACP_TOOLS=1
export ACP_TOOL_ADDR=192.168.64.1:0                    # 工具面监听网关（默认 127.0.0.1:0）
export ACP_TOOL_WORKDIR=$HOME/.wecom-aibot/acp-work
export ACP_HOSTFWD=3425:3425                           # 容器把 3425 交给宿主的 magpie：
                                                       # bot 进程内网关→回环逐端口转发

# pi 配置整体放挂载目录（PI_CODING_AGENT_DIR），models.json 的 baseUrl 写
# http://192.168.64.1:3425/v1——端口与宿主原端口一致，只换主机名。

export ACP_COMMAND=container
export ACP_ARGS="run --rm -i \
  --user 501:20 \
  -v $HOME/.wecom-aibot/acp-work:$HOME/.wecom-aibot/acp-work \
  -e HOME=$HOME/.wecom-aibot/acp-work/home -w $HOME/.wecom-aibot/acp-work/home \
  -e PI_CODING_AGENT_DIR=$HOME/.wecom-aibot/acp-work/.pi-agent \
  pi-agent:26 pi-acp"

go run ./examples/acp-bot
```

## docker / OrbStack（平滑回退：Linux VM 内共享内核容器）

工具面回环监听 + 通告 `host.docker.internal`（OrbStack 把它转发到宿主 loopback）：

```bash
export ACP_TOOLS=1
export ACP_MCP_ADVERTISE_URL=http://host.docker.internal   # 只影响写进 mcp.json 的 URL

export ACP_COMMAND=docker
export ACP_ARGS="run --rm -i \
  --user 501:20 \
  -v $HOME/.wecom-aibot/acp-work:$HOME/.wecom-aibot/acp-work \
  -e HOME=$HOME/.wecom-aibot/acp-work/home -w $HOME/.wecom-aibot/acp-work/home \
  -e PI_CODING_AGENT_DIR=$HOME/.wecom-aibot/acp-work/.pi-agent \
  pi-agent:26 pi-acp"

go run ./examples/acp-bot
```

models.json 的 baseUrl 写 `http://host.docker.internal:3425/v1`。

## 要点备忘（实测得出）

- **同径挂载**：workDir 用宿主绝对路径原样挂进容器，`.pi/mcp.json`、session/new 的 cwd、
  附件落盘路径内外一致，零路径重写。
- **`--user 501:20`**：容器进程与宿主同 uid，挂载目录属主对齐（P1 曾踩 uid 错位 chmod
  不救的坑）。`501` 换成你的 `id -u`。
- **HOME/PI_CODING_AGENT_DIR 落挂载区**：容器内 HOME 缺省不可写（非 root 无属主），
  显式指到挂载目录内；pi 配置（models/settings/trust）随之宿主侧可直接编辑。
- **Apple container 网关 IP `192.168.64.1`**：多次 run 实测稳定；若因网络重建漂移，
  `container run --rm alpine:latest ip route` 看一眼默认网关，必要时
  `ACP_HOSTFWD_BIND=<新网关>` 校正。
- **`ACP_HOSTFWD`（回环转发归宿 B）**：外部服务只听 127.0.0.1 时（magpie 类），bot
  进程内把网关端口转发到回环——服务零改动，且转发与 bot 同生命周期（不存在
  「转发器挂了 bot 还活着」的半瘫态）。若外部服务自己能 bind 网关（归宿 A），则连
  这行也不需要。
- 官方替代路径 `sudo container system dns create host.container.internal --localhost …`
  要 sudo + pf（禁用 iCloud Private Relay、重启丢失）——已避开，不需要。
- 不设这些 env、`ACP_COMMAND=pi-acp` 直跑时，行为与无沙箱部署完全一致（沙箱纯叠加）。

## 完整 E2E 实测（2026-10-17，Apple container 路线）

生产形态全链路跑通：企微消息 → acp-bot（WS）→ `container run -i` microVM 内
pi-acp → 网关转发调宿主 LLM → 流式回复；pi 在容器内 `uname` 自报 Linux、
宿主仅 workDir 可见，经 `.pi/mcp.json` + Bearer 成功调用工具面 `send_template_card`
（`ok:true`，卡片落群）。两个行为备忘：

1. **pi 未径直走原生 MCP 工具**：pi-acp 0.0.34 的 `mcpCapabilities.http=false`
   （容器内 pi 不自动加载项目 mcp.json 的 MCP 客户端），实测里 pi 读到
   mcp.json 后选择了 bash+node 手写 HTTP 调端点——端点鉴权与执行完全正常，
   只是绕了一层。依赖原生 `mcp__wecom__*` 工具名时，需 pi-acp 上游支持
   http MCP（本仓库注记：MCP 端点与鉴权契约已就绪，升级即受益）。
2. **read_attachment 的"当前消息没有附件"**：附件读取绑定的快照是"当前在途
   消息"，卡片消息成功后紧跟的文件消息若尚未进入 prompt 在途，工具会如实
   返回无附件自纠文案。复测"先发文件再一句话指令"即可正常统计；该行为
   与无沙箱模式一致（快照生命周期设计使然），非沙箱引入的回归。
