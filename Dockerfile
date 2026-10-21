# mars-agent 运行时镜像 = 钉版 opencode 二进制 + mars-agent wrapper + 内置只读 Skills。
#
# ⚠️ opencode 二进制**不在此镜像内拉取**：镜像里 curl|bash 或裸下二进制是不可审计的供应链风险。
# 二进制由 `README.md` 描述的步骤，以"钉版本 + 校验 SHA256"的方式预置到构建上下文
# vendor/opencode/opencode-linux-{amd64,arm64}，再由本文件 COPY 进来（构建上下文 = 仓库根）。
# 生产环境应进一步以镜像 digest + cosign 验签分发。

# 构建阶段固定跑在构建机架构（BUILDPLATFORM），由 Go 交叉编译到 TARGETARCH：
# 多架构 buildx 下不会让 arm64 的整套 Go 工具链跑在 QEMU 模拟里，编译接近原生速度。
# 产物仍是纯静态的 $TARGETARCH 二进制（CGO_ENABLED=0），运行阶段不受影响。
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /src

# 只拷编译所需输入：Go 源码 + 两个 //go:embed 目录（skills/、tui/）。
# 不用 COPY . .：vendor/opencode 下两份二进制共约 492MB，拷进 builder 只会拖慢
# 每次源码变更后的重建；产物只依赖这几十 KB 源码。
# 本模块没有外部依赖（go.mod 无 require），故不需要 go mod download 层。
COPY go.mod *.go ./
COPY skills ./skills
COPY tui ./tui

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w" -o /out/mars-agent .

# 运行时基座：debian-slim（glibc，与预置 opencode 二进制 ABI 一致）+ 一组精选常用排查工具。
# 不用 alpine/musl——opencode 是 glibc 变体，musl 下跑不起来。
# 默认非 root（uid 65532），与 Pod 安全上下文 runAsUser 对齐。
#
# ⚠️ 权衡：工具经 apt 安装、版本随 Debian 12 仓库浮动（未逐包钉死），与"钉版 opencode + 校验和"
#    的可复现口径不同。要工具面就要接受这点；若要绝对可复现，改用 digest 钉死的自建基础镜像。
FROM debian:12-slim

ARG TARGETARCH

# apt 源默认走上游；受限网络（本机实测 deb.debian.org 拉 main 索引超时）可换国内镜像：
#   docker build --build-arg APT_MIRROR=mirrors.ustc.edu.cn ./agent
ARG APT_MIRROR=deb.debian.org

# 只装排查/调试必需的少量工具（base 已有 coreutils/grep/sed/mawk）：
# shell、进程、网络、TLS 证书、JSON、文本、压缩。--no-install-recommends + 清 apt 列表控体积。
RUN set -eux; \
    for f in /etc/apt/sources.list /etc/apt/sources.list.d/debian.sources; do \
        [ -f "$f" ] && sed -i "s|deb.debian.org|${APT_MIRROR}|g" "$f" || true; \
    done; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        bash ca-certificates curl wget \
        procps iproute2 netcat-openbsd dnsutils \
        jq less vim-tiny file lsof \
        tar gzip unzip; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd -g 65532 nonroot; \
    useradd -u 65532 -g 65532 -M -d /workspace -s /bin/bash nonroot; \
    install -d -o 65532 -g 65532 /workspace

# 交互式 shell 提示符：K8s Pod 名很长（agent-<uid>-mars-agent-<hash>-<pod>），
# 默认 \h 会把提示符撑爆。固定成短名，只保留当前路径（不显示用户名）。
# 写 /etc/bash.bashrc 而不是 ~/.bashrc：/workspace 在 K8s 里被 emptyDir 覆盖。
RUN printf '%s\n' 'PS1="mars-agent:\w\$ "' >> /etc/bash.bashrc

WORKDIR /workspace

# 按目标架构拷入对应的 glibc 版 opencode 二进制（预置在构建上下文；见 README.md）。
COPY vendor/opencode/opencode-linux-${TARGETARCH} /usr/local/bin/opencode
# 用户面向的稳定入口 mai：底层 agent 由 MARS_AI_BIN 指定（默认 opencode），
# 以后换 claude code 等内核时用户命令不变。
COPY scripts/mai.sh /usr/local/bin/mai
RUN chmod 0755 /usr/local/bin/mai
# mars-agent wrapper（Skills 已 //go:embed 进二进制）
COPY --from=builder /out/mars-agent /usr/local/bin/mars-agent
# 入口脚本：启动时用 gatewayURL + token 拉取 mars 配置（模型）注入环境，再交给 wrapper。
# 依赖基础镜像已装的 curl 与 jq（见上方 apt 安装列表）。
COPY scripts/entrypoint.sh /usr/local/bin/mars-agent-entrypoint

ENV HOME=/workspace

USER 65532:65532

ENTRYPOINT ["/usr/local/bin/mars-agent-entrypoint"]
