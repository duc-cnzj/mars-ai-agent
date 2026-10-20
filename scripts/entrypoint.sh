#!/bin/sh
# mars-agent 容器入口脚本：启动时先用 gatewayURL + 会话令牌向 mars 拉取运行时配置，
# 再把模型注入 MARS_MODEL 环境变量，最后交给 mars-agent wrapper。
#
# 契约（与 mars 侧 internal/services/aiconfig.go 对齐）：
#   GET ${MARS_GATEWAY_URL}/api/ai/config
#   Authorization: Bearer ${MARS_SESSION_TOKEN}
#   200 → {"model":"<裸模型 id>"}
#
# wrapper 随后生成 $HOME/.config/opencode/opencode.json（含模型、LLM/MCP 地址）并
# exec opencode serve。因此用户之后进入 opencode 无需任何额外参数——配置已就绪。
#
# MARS_MODEL 已显式设置时跳过拉取（本地联调 / 离线调试逃生舱）。
# 令牌只用于请求头，不打印、不落盘。
set -eu

: "${MARS_GATEWAY_URL:?缺少必填环境变量}"
: "${MARS_SESSION_TOKEN:?缺少必填环境变量}"

if [ -z "${MARS_MODEL:-}" ]; then
  url="${MARS_GATEWAY_URL%/}/api/ai/config"

  # 重试策略：网关刚起来或网络抖动时，单次失败不应让沙箱直接 CrashLoop。
  # 只有网络错误、5xx 与 429 视为瞬时可重试；其余 4xx 是配置/鉴权问题，重试无意义。
  max_attempts=3
  attempt=1
  last_err=""
  while :; do
    # 保留响应体与状态码：失败时把 mars 的说明原样带出来，便于定位是未配置还是鉴权失败。
    if resp="$(curl -sS --max-time 10 -w '\n%{http_code}' \
      -H "Authorization: Bearer ${MARS_SESSION_TOKEN}" "$url")"; then
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

# MARS_AGENT_BIN 仅供本地测试/调试替换被 exec 的二进制，生产镜像默认无需设置。
exec "${MARS_AGENT_BIN:-/usr/local/bin/mars-agent}" "$@"
