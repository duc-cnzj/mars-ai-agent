#!/bin/sh
# mars-agent 容器入口脚本：启动时先用 gatewayURL + 会话令牌向 mars 拉取运行时配置，
# 再把模型注入 MARS_MODEL 环境变量，最后交给 mars-agent wrapper。
#
# 契约（与 mars 侧 internal/services/aiconfig.go 对齐）：
#   GET ${MARS_GATEWAY_URL}/api/ai/config
#   Authorization: Bearer ${MARS_SESSION_TOKEN}
#   200 → {"model":"<裸模型 id>"}
#
# 灰度用户（GET ${MARS_GATEWAY_URL}/api/auth/info 的 isGray=true）会额外把
# MARS_GRAY=always 注入环境，wrapper 据此在所有发往 mars 的请求上带 mars_gray 头。
#
# wrapper 随后生成 $HOME/.config/opencode/opencode.json（含模型、LLM/MCP 地址）并
# exec opencode serve。因此用户之后进入 opencode 无需任何额外参数——配置已就绪。
#
# MARS_MODEL 已显式设置时跳过拉取（本地联调 / 离线调试逃生舱）。
# 令牌只用于请求头，不打印、不落盘。
set -eu

: "${MARS_GATEWAY_URL:?缺少必填环境变量}"
: "${MARS_SESSION_TOKEN:?缺少必填环境变量}"

# 灰度通道探测：灰度标记不在 JWT 里（mars 侧每次读库），沙箱只能在启动时问一次。
# 探测请求本身还没法带灰度头（此时不知道结果），但拿到结果后，后续配置、LLM 代理与
# MCP 请求都会带上 mars_gray，由 nginx-ingress canary 固定路由到灰度通道。
# MARS_GRAY 已显式设置时跳过探测（本地联调逃生舱）。
# 探测失败按非灰度处理（与 mars /api/auth/info 的 fail-open 口径一致），不阻断启动。
if [ -z "${MARS_GRAY:-}" ]; then
  info_url="${MARS_GATEWAY_URL%/}/api/auth/info"
  gray=""
  if info="$(curl -sS --max-time 10 -H "Authorization: Bearer ${MARS_SESSION_TOKEN}" "$info_url")"; then
    if [ "$(printf '%s' "$info" | jq -r '.isGray // false' 2>/dev/null)" = "true" ]; then
      gray="always"
    fi
  else
    echo "mars-agent: 探测灰度通道失败，按非灰度启动: ${info_url}" >&2
  fi
  MARS_GRAY="$gray"
  export MARS_GRAY
  echo "mars-agent: 灰度通道探测结果: ${MARS_GRAY:-非灰度}"
fi

if [ -z "${MARS_MODEL:-}" ]; then
  url="${MARS_GATEWAY_URL%/}/api/ai/config"

  # 灰度用户的所有 mars 请求都带路由头；非灰度不带（空值头没有意义）。
  fetch_config() {
    if [ -n "${MARS_GRAY:-}" ]; then
      curl -sS --max-time 10 -w '\n%{http_code}' \
        -H "Authorization: Bearer ${MARS_SESSION_TOKEN}" \
        -H "mars_gray: ${MARS_GRAY}" "$url"
    else
      curl -sS --max-time 10 -w '\n%{http_code}' \
        -H "Authorization: Bearer ${MARS_SESSION_TOKEN}" "$url"
    fi
  }

  # 重试策略：网关刚起来或网络抖动时，单次失败不应让沙箱直接 CrashLoop。
  # 只有网络错误、5xx 与 429 视为瞬时可重试；其余 4xx 是配置/鉴权问题，重试无意义。
  max_attempts=3
  attempt=1
  last_err=""
  while :; do
    # 保留响应体与状态码：失败时把 mars 的说明原样带出来，便于定位是未配置还是鉴权失败。
    if resp="$(fetch_config)"; then
      status="$(printf '%s' "$resp" | tail -n 1)"
      body="$(printf '%s' "$resp" | sed '$d')"
      if [ "$status" = "200" ]; then
        break
      fi
      case "$status" in
        4??)
          if [ "$status" != "429" ]; then
            echo "mars-agent: mars 配置接口返回 ${status}: ${body}" >&2
            exit 1
          fi
          ;;
      esac
      last_err="mars 配置接口返回 ${status}: ${body}"
    else
      last_err="拉取 mars 配置失败（网络错误）: ${url}"
    fi

    if [ "$attempt" -ge "$max_attempts" ]; then
      echo "mars-agent: ${last_err}（已尝试 ${attempt} 次）" >&2
      exit 1
    fi
    echo "mars-agent: 第 ${attempt} 次拉取配置失败：${last_err}；1 秒后重试" >&2
    attempt=$((attempt + 1))
    sleep 1
  done

  model="$(printf '%s' "$body" | jq -r '.model // empty')"
  if [ -z "$model" ]; then
    echo "mars-agent: mars 配置接口未返回 model: ${body}" >&2
    exit 1
  fi

  export MARS_MODEL="$model"
  echo "mars-agent: 已从 mars 拉取运行配置（model=${model}）"
fi

# MCP 预热：opencode serve 起来后在后台持续确保 mars-tools 已连接。
#
# opencode 启动时会自己尝试连一次 MCP，但网关/网络尚未就绪时会失败且状态粘住，
# 网关恢复后也不会自动重连。这里在容器启动后立刻异步预热并重试，用户打开终端
# 时通常已就绪，避免每次进 TUI 都现场等待。预热脚本会先等 server 的 /health，
# 因此可以安全地放在 exec 之前；后台任务不阻塞容器启动。
# 等待上限可用 MARS_MCP_PREWARM_TIMEOUT 调整（秒，0 表示不预热）。
# best-effort：预热失败只影响工具可用性，不影响 shell / TUI 启动。
prewarm="${MARS_MCP_PREWARM_TIMEOUT:-120}"
mcp_wait_bin="${MARS_MCP_WAIT_BIN:-/usr/local/bin/mars-mcp-wait}"
ai_bin="${MARS_AI_BIN:-/usr/local/bin/opencode}"
if [ "$(basename "$ai_bin")" = "opencode" ] &&
  [ "$prewarm" -gt 0 ] 2>/dev/null && [ -x "$mcp_wait_bin" ]; then
  "$mcp_wait_bin" \
    --dir "${HOME:-/workspace}" \
    --timeout "$prewarm" \
    --server-timeout 60 &
fi

# MARS_AGENT_BIN 仅供本地测试/调试替换被 exec 的二进制，生产镜像默认无需设置。
exec "${MARS_AGENT_BIN:-/usr/local/bin/mars-agent}" "$@"
