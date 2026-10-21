# mars-agent（沙箱侧 AI 助手）

mars「AI 容器排查助手」的**沙箱侧组件**。每个用户独占一个沙箱（单副本 Deployment），
沙箱内运行本镜像：它以 **opencode** 为 agent 内核，把 LLM 调用与工具调用统统导回 mars。

## 职责（只做三件事）

1. 校验 mars 注入的环境变量；入口脚本先用网关地址 + 令牌向 mars 拉取运行时配置（模型）
2. 生成 opencode 配置：LLM provider 指向 mars LLM 代理、MCP 工具指向 mars 工具服务
3. 落盘知识型 Skills，然后 `exec` 成常驻的 `opencode serve`

> 配置只有一个来源：mars 的 `ai` 配置段。chart 只下发 `mars.gatewayURL` 与会话令牌，
> agent 启动时经 `GET <gateway>/api/ai/config` 拿到模型，因此进入 opencode 无需任何额外参数。

> LLM 循环由 opencode 自己跑，mars 服务端不重复实现 agent。用户在项目终端里通过
> `mai` 直接使用沙箱内的 opencode TUI，mars 以 MCP 形式向它暴露受 RBAC + 人工审批
> 约束的工具。
> 本模块**不 import mars 任何代码**，与 mars 之间只经 MCP / HTTP 交互。

## 环境变量契约（由沙箱 Pod manifest 注入）

| 变量 | 必填 | 默认 | 用途 |
|---|---|---|---|
| `MARS_GATEWAY_URL` | ✅ | — | mars 网关地址（MCP + LLM 代理 + 运行时配置），如 `http://mars-server.mars.svc:9000` |
| `MARS_SESSION_TOKEN` | ✅ | — | 短期 scoped JWT，MCP、LLM 与配置接口共用；**绝不接受用户真实 token** |
| `MARS_MODEL` | | 由入口脚本拉取 | 模型 id（裸 id，非 provider 限定名；配置里会写成 `mars/<id>`）。显式设置时跳过拉取 |
| `MARS_GRAY` | | 由入口脚本探测 | 灰度通道标记（`always`=灰度用户，空=非灰度）。非空时 LLM 与 MCP 请求都带 `mars_gray` 头；显式设置时跳过探测 |
| `MARS_AI_BIN` | | `/usr/local/bin/opencode` | 底层 agent 二进制路径（`mai` 与服务进程共用；可替换成 claude code 等） |
| `MARS_LISTEN_ADDR` | | `0.0.0.0:4096` | opencode serve 监听地址 |
| `MARS_MCP_WAIT_TIMEOUT` | | `15` | `mai` 进入 TUI 前等待 MCP 就绪的上限（秒，0=不等待） |
| `MARS_MCP_PREWARM_TIMEOUT` | | `120` | 容器启动后后台预热 MCP 的上限（秒，0=不预热） |
| `HOME` | | `/workspace` | 配置与 skills 的落盘根（`$HOME/.config/opencode/`） |

入口脚本 `scripts/entrypoint.sh` 在启动时调用 mars 配置接口补齐 `MARS_MODEL`：

```text
GET <MARS_GATEWAY_URL>/api/ai/config
Authorization: Bearer <MARS_SESSION_TOKEN>
200 → {"model":"<裸模型 id>"}
```

非 200、响应无法解析或 `model` 为空都会打印原因并以非 0 退出（fail fast），
不让沙箱带着坏配置起来。接口的服务端实现与路径契约见 mars 仓 `internal/services/aiconfig.go`。

启动时还会探测一次灰度通道，决定后续所有发往 mars 的请求是否带 `mars_gray` 头：

```text
GET <MARS_GATEWAY_URL>/api/auth/info
Authorization: Bearer <MARS_SESSION_TOKEN>
200 → {"isGray":true|false,...}
```

`isGray=true` 时入口脚本导出 `MARS_GRAY=always`，wrapper 把它写进 opencode 配置：
LLM 供应商的 `options.headers` 与 MCP 服务的 `headers` 各带一份 `mars_gray: always`，
由 nginx-ingress canary 把配置、LLM 代理与 MCP 请求固定路由到灰度通道。
探测失败按非灰度处理（与 mars `/api/auth/info` 的 fail-open 口径一致），不阻断启动；
探测请求本身是唯一无法携带该头的 mars 请求（此时还不知道结果）。
**灰度标记不进 JWT**（mars 每次读库），所以沙箱只在容器启动时感知：后台改标后，
存量沙箱需重启（缩容后再次唤醒即可重新触发探测）才会切换通道。

## MCP 就绪保证

opencode 只在 instance 创建时连一次 MCP，失败后状态会粘住（`status=failed`），
网关恢复后也不会自动重连；且 `POST /mcp/<name>/connect` 只发起连接、不等待完成。
因此启动链路分两层保证：

1. **容器启动预热**：入口脚本在 `exec` 前拉起后台 `mars-mcp-wait`，等 opencode
   `/health` 可达后重连 MCP 并轮询到 `connected`（默认最多 120s，可重试覆盖
   网关尚未就绪的情况）。用户打开终端时通常已就绪。
2. **进入 TUI 前兜底**：`mai` attach 前再等待一次（默认 15s），覆盖非
   `/workspace` 目录、常驻 server 重启、预热超时等场景。

两层都是 best-effort：超时只告警、不阻断，网关整体故障时用户仍可进入终端写代码。

## 目录结构

```
.
├── scripts/                        镜像内 shell 脚本
│   ├── entrypoint.sh               容器入口：拉取 mars 配置、后台预热 MCP，再交给 wrapper
│   ├── mai.sh                      用户面向的稳定入口（mai → MARS_AI_BIN，默认 opencode；attach 前等待 MCP 就绪）
│   └── mcp-wait.sh                 MCP 就绪等待（轮询 /mcp + connect），启动预热与 TUI 兜底共用
├── main.go                         入口：校验 → 落配置/skills → exec 底层 agent serve
├── config.go                       生成 opencode.json（LLM/MCP 指向 mars）
├── skills.go                       //go:embed skills + 只读落盘
├── skills/                         知识型 Skills（纯知识，无执行权）
│   ├── mars-resource-model/SKILL.md
│   └── crashloop-triage/SKILL.md
├── tui/                            opencode TUI 定制（tui.json + 启动页 logo / 能力清单插件）
├── Dockerfile                      镜像：钉版 opencode + wrapper + skills
└── vendor/opencode/                预置的 opencode 二进制（git 忽略，见下）
```

## 本地构建与测试

```bash
make agent          # 产出 bin/mars-agent
make agent_test     # go test ./... -race
make agent_check    # 本地门禁：gofmt + go vet + 全量测试（与 release 的 test job 对齐）
```

冒烟（用替身避免真拉起 opencode）：

```bash
MARS_AI_BIN=/bin/echo \
MARS_GATEWAY_URL=http://mars-server.mars.svc:9000 \
MARS_SESSION_TOKEN=tok MARS_MODEL=demo \
HOME=$(mktemp -d) ./bin/mars-agent
# 随后检查 $HOME/.config/opencode/opencode.json 与 skills/ 是否正确生成
```

入口脚本可单独验证（`MARS_AGENT_BIN` 仅用于替换被 exec 的二进制）：

```bash
MARS_GATEWAY_URL=http://127.0.0.1:4000 MARS_SESSION_TOKEN=tok \
MARS_AGENT_BIN=/bin/echo sh scripts/entrypoint.sh ok
```

## CI/CD（GitHub Actions）

| 工作流 | 触发 | 做什么 |
|---|---|---|
| `.github/workflows/release.yml` | 打 `v*` tag | 发布门禁（gofmt → `go vet` → `go test -race` → `make agent`，并对 chart 跑 `helm lint` + 渲染断言），通过后构建 `linux/amd64,linux/arm64` 镜像并推送（口径与 mars 主仓一致） |

> 仓库只有发布流水线，没有 PR / 分支 CI：合入前的检查请在本地跑
> `make agent_test`，chart 改动再跑一遍下文「Helm 部署」里的 `helm lint` / `helm template`。

发布产物（与 chart 默认 `image.repository` 一致）：

- `registry.cn-hangzhou.aliyuncs.com/duc-cnzj/mars-agent:<tag>`
- Docker Hub `duccnzj/mars-agent:<tag>`

每次发布同时更新 `vX.Y`（由 `vX.Y.Z` 派生）与浮动 `latest`。

### macOS（Apple Silicon）

macOS 上**没有独立的 macOS 镜像**：Docker Desktop / OrbStack 在 Mac 上运行的是 Linux VM，
Apple Silicon 机器实际拉取并运行的是 `linux/arm64` 镜像。发布的多架构 manifest 已包含该架构，
无需额外 tag：

```bash
docker pull registry.cn-hangzhou.aliyuncs.com/duc-cnzj/mars-agent:<tag>   # 自动选择 linux/arm64
# 如需显式指定：
docker run --platform linux/arm64 ... registry.cn-hangzhou.aliyuncs.com/duc-cnzj/mars-agent:<tag>
```

发布前需在仓库 Secrets 配置镜像仓库凭据（与 mars 主仓同名）：

| Secret | 用途 |
|---|---|
| `ALIYUN_DOCKER_USERNAME` | 阿里云镜像仓库用户名 |
| `ALIYUN_DOCKER_PASSWORD` | 阿里云镜像仓库密码 |
| `DOCKER_USERNAME` | Docker Hub 用户名 |
| `DOCKER_PASSWORD` | Docker Hub 密码 / access token |

发布步骤：

```bash
# 先把 Chart.yaml 的 version / appVersion 提升到同一版本（release 会校验一致性）
git tag v0.1.0 && git push origin v0.1.0   # 触发 release.yml
```

> release 工作流里的镜像构建同样走 `make agent_opencode`，**逐个校验 sha256**——钉版本不校验和等于没钉。
> 首次发布后建议在仓库 Settings → Actions → General 确认 workflow 权限允许
> `Read and write permissions`（或至少允许 packages: write）。

## 进容器终端

运行时基座是 debian-slim + 少量常用排查工具（bash / ps / ip / ss / nc / dig / curl / wget /
jq / less / vi / file / lsof / tar / unzip），非 root（uid 65532）。直接 exec 进去：

```bash
docker run -d --name mars-agent -p 4096:4096 \
  -e MARS_GATEWAY_URL=http://host.docker.internal:4000 \
  -e MARS_SESSION_TOKEN=<令牌> \
  mars-agent:dev
docker exec -it mars-agent bash
```

> 模型由入口脚本从 mars 的 `/api/ai/config` 拉取，无需 `-e MARS_MODEL`；
> 仅在脱离 mars 调试时才显式设置它覆盖拉取结果。

进去后 wrapper 已把配置写在 `$HOME/.config/opencode/`，直接可跑（**无需任何额外准备**）：

```bash
mai                  # 交互式 TUI
mai run "你好"        # 非交互跑一句
```

> `mai` 是镜像内置的稳定入口（`/usr/local/bin/mai`），底层实际执行
> `$MARS_AI_BIN`（默认 `/usr/local/bin/opencode`）。以后换底层内核（如 claude code）
> 只需改这个环境变量指向新二进制，用户命令与本文档都不用动；`opencode` 仍可直接使用。

> 想让 LLM 指向真实端点而非 mars 代理，加 `-e MARS_LLM_BASE_URL=<openai兼容>/v1`
> 并把令牌换成该端点 key——**改 env 即可，不改代码不重编**。
> 受限网络构建时换 apt 源：`make agent_image APT_MIRROR=mirrors.ustc.edu.cn`。

## Helm 部署

chart 位于 `charts/mars-agent/`，每个用户一个 release = 一个 Deployment（端口 4096，
不创建 Service）。用户经 mars 的项目终端（WebSocket → `pods/exec`）进入沙箱使用
`mai`，浏览器不直连沙箱、不经 HTTP 反代；集群内调试走 `kubectl port-forward`（见 NOTES）。

```bash
helm install alice ./charts/mars-agent \
  --namespace mars-ai-alice --create-namespace \
  --set mars.token=<mars 注入的短期 JWT>
```

配置项：

| 值 | 说明 |
|---|---|
| `mars.gatewayURL` | mars 网关地址（默认 `http://mars-server.mars.svc:9000`） |
| `mars.token` | 会话令牌（短期 scoped JWT）；为空则安装直接失败（fail fast） |
| `image.repository` | 镜像仓库（默认 `registry.cn-hangzhou.aliyuncs.com/duc-cnzj/mars-agent`） |
| `image.tag` | 镜像 tag（默认取 `Chart.appVersion`；`values.yaml` 中留空） |
| `image.pullPolicy` | 拉取策略（默认 `IfNotPresent`） |
| `imagePullSecrets` | 私有仓库拉取凭据（可选） |
| `podAnnotations` | 追加到 Pod 的注解 |
| `resources` | 资源请求/限制 |
| `nodeSelector` / `tolerations` / `affinity` | 调度约束 |

> 由 mars 平台托管时（AI Agent 仓库部署到 `mars-agent` 空间），仓库 values 中的
> `mars.token` **必须**写成 `"<.UserToken>"`：部署流水线会在每次部署/唤醒时替换为
> 当前用户签发的短期令牌并写入 Secret。未引用该变量时 mars 的启用入口会直接拒绝，
> 因为沙箱能起来但所有回连都会 401。

模型等运行时参数**不在 chart 里配置**：agent 启动时经 `GET <gatewayURL>/api/ai/config`
从 mars 拉取（模型在 mars 的 `ai.model` 维护）。端口 4096、Recreate 更新策略、
非 root（uid 65532）、根文件系统只读（可写面只有 `/workspace` 与 `/tmp`）、
探针、不挂载 ServiceAccount token 等为内置固定值。

本地校验：

```bash
helm lint charts/mars-agent --set mars.token=demo-token
helm template smoke charts/mars-agent --set mars.token=demo-token
```

> 核对渲染结果：**不应出现 Service**（无反向代理入口），副本固定为 1，镜像来自默认仓库，且不注入 `MARS_MODEL`。

## 预置 opencode 二进制（构建镜像前必做）

镜像**不做** `curl | bash`。请以钉死版本 + 校验和的方式预置：

1. 确定要钉的 opencode 版本，从其 release 页取得对应架构（linux amd64/arm64）的二进制与 SHA256。
2. 将二进制放到 `vendor/opencode/opencode-linux-{amd64,arm64}`，并**核对校验和**
   （或用 `make agent_opencode` 按 Makefile 中钉死的版本与 digest 自动下载、校验并落位）。
3. `make agent_image` 构建镜像。

> ⚠️ 只钉版本不校验和 = 没钉。生产分发应进一步以镜像 digest + cosign 验签。

## 与 opencode 版本强耦合的点（升级时必查）

- `config.go` 的配置字段形态：v1 用 `provider` / `npm` / `options`，v2 用 `providers` / `package` / `settings`。
- `config.go` 的 `permission` 规则键：opencode 把 MCP 工具注册为 `sanitize(服务名)_sanitize(工具名)`
  （当前为 `mars-tools_<tool>`）；且**不消费** MCP 的 `readOnlyHint`，写工具未显式配 `ask` 时会默认 allow。
- `main.go` 的 `opencode serve` 参数名（`--hostname` / `--port`）。
- Skills 的发现路径（`$HOME/.config/opencode/skills/`）。
- `tui/plugins/mars-logo.tsx` 使用的 `home_logo` 插槽与 TUI 插件 API（TUI 配置为 `tui.json`）。
- `tui.go` 的 `DisableTips` 依赖 TUI 键值存储路径（`$XDG_STATE_HOME/opencode/kv.json`，
  默认 `$HOME/.local/state/opencode/kv.json`）与内置 tips 的开关键 `tips_hidden`。
- chart 的 startup / readiness / liveness 探针都打 opencode 的 `/health`（`deployment.yaml`），
  路径变化会让 Pod 永远不就绪。

六者都已在代码内集中标注，升级只需改动对应单点。

## 安全设计要点

- **密钥不落盘**：配置里的令牌一律写 opencode 的 `{env:MARS_SESSION_TOKEN}` 占位符。
- **Skills 只读**：落盘后文件 0444、目录 0555，压缩运行期被诱导写入恶意 skill 的面。
- **无集群凭证**：本组件不访问 Kubernetes API，仅经 mars 网关间接操作容器。
