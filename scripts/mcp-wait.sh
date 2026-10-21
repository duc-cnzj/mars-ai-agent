#!/bin/sh
# mars-mcp-wait：等待 opencode 的 MCP 工具服务（mars-tools）进入 connected。
#
# 背景：opencode 只在 instance 创建时连一次 MCP，失败后状态会粘住
# （日志表现为 "server unavailable ... status=failed"），网关恢复后也不会自动
# 重连；且 connect API 只发起连接、不等待完成。因此必须轮询 /mcp 状态。
#
# 用法：
#   mars-mcp-wait [--dir DIR] [--timeout SEC] [--server-timeout SEC]
#
#   --dir DIR             instance 目录（默认 $PWD），与 TUI 的 --dir 一致
#   --timeout SEC         server 就绪后等待 MCP connected 的上限（默认取
#                         MARS_MCP_WAIT_TIMEOUT，未设置则 15；0 表示只探测一次）
#   --server-timeout SEC  等待 opencode /health 可达的上限（默认 0：只探测一次）
#
# 退出码：0 = connected；1 = 超时仍未就绪；2 = opencode server 不可达。
set -u

dir="$PWD"
timeout="${MARS_MCP_WAIT_TIMEOUT:-15}"
server_timeout=0
# 与 config.go 的 mcpServerName 保持一致。
mcp_server="mars-tools"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --dir)
      dir="$2"
      shift 2
      ;;
    --timeout)
      timeout="$2"
      shift 2
      ;;
    --server-timeout)
      server_timeout="$2"
      shift 2
      ;;
    *)
      echo "mars-mcp-wait: 未知参数 $1" >&2
      exit 64
      ;;
  esac
done

case "$timeout" in
  '' | *[!0-9]*) timeout=15 ;;
esac
case "$server_timeout" in
  '' | *[!0-9]*) server_timeout=0 ;;
esac

addr="${MARS_LISTEN_ADDR:-0.0.0.0:4096}"
# 监听地址可能是 0.0.0.0/[::]，容器内访问统一走回环。
url="http://127.0.0.1:${addr##*:}"

# 先等 server 可达：entrypoint 预热时 opencode 可能还没起来。
health_deadline=$(( $(date +%s) + server_timeout ))
while ! curl -sf -o /dev/null --max-time 1 "${url}/health" 2>/dev/null; do
  if [ "$(date +%s)" -ge "$health_deadline" ]; then
    echo "mars-mcp-wait: opencode server 未在 ${server_timeout}s 内就绪: ${url}" >&2
    exit 2
  fi
  sleep 1
done

# 查询 /mcp 本身会按 directory 创建 instance 并触发首次连接。
probe() {
  body="$(curl -sS -G --max-time 3 \
    --data-urlencode "directory=$dir" "${url}/mcp" 2>/dev/null)" || {
    status=""
    error=""
    return 1
  }
  status="$(printf '%s' "$body" |
    jq -r --arg n "$mcp_server" '.[$n].status // empty' 2>/dev/null)"
  error="$(printf '%s' "$body" |
    jq -r --arg n "$mcp_server" '.[$n].error // empty' 2>/dev/null)"
}

status=""
error=""
probe || true
[ "$status" = "connected" ] && exit 0
[ "$timeout" -eq 0 ] && exit 1

echo "mars-mcp-wait: 等待 MCP 工具服务就绪（${mcp_server}，dir=${dir}）..." >&2
deadline=$(( $(date +%s) + timeout ))
while [ "$status" != "connected" ]; do
  now="$(date +%s)"
  [ "$now" -ge "$deadline" ] && break
  # connect 可能阻塞到本次尝试结束（网关不可达时会很快失败）；给它剩余预算
  # 但至少 1 秒，避免 5 秒就把尚未完成的重连掐断。
  remaining=$(( deadline - now ))
  [ "$remaining" -lt 1 ] && remaining=1
  curl -sS -o /dev/null --max-time "$remaining" -G -X POST \
    --data-urlencode "directory=$dir" \
    "${url}/mcp/${mcp_server}/connect" >/dev/null 2>&1 || true
  probe || true
  [ "$status" = "connected" ] && exit 0
  [ "$(date +%s)" -ge "$deadline" ] && break
  sleep 1
done

echo "mars-mcp-wait: MCP 工具服务未在 ${timeout}s 内就绪（status=${status:-unknown}${error:+; ${error}}）" >&2
exit 1
