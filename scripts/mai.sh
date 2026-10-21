#!/bin/sh
# mai：容器内面向用户的稳定 AI agent 入口。
#
# 用户统一用 `mai`（交互式 TUI）或 `mai run "..."`（非交互），
# 不直接依赖底层实现。底层 agent 由 MARS_AI_BIN 指定，默认 opencode；
# 以后替换成 claude code 等其他内核时，只需改这个环境变量（或镜像内默认值），
# 用户命令与文档都不用动。
#
# 与 wrapper 的关系：常驻服务由 mars-agent wrapper exec 底层 agent 提供，
# 本脚本只负责交互式用法，二者读同一个 MARS_AI_BIN，避免指向漂移。
set -eu

bin="${MARS_AI_BIN:-/usr/local/bin/opencode}"

# 交互式 TUI 默认复用常驻 server（`opencode attach`），而不是让 TUI 在进程内
# 再起一个 server。实测同一台机器上空闲状态：
#   直接启动 TUI ≈ 1.1 GiB RSS / ~5% CPU；attach 客户端 ≈ 380 MiB RSS / ~1% CPU。
# 沙箱内存通常 2 GiB，双份 server 叠加会把进程压进换页，表现为输入/滚动卡顿。
# 只在底层是 opencode 时启用复用；换其他内核时命令与参数原样透传。
if [ "$(basename "$bin")" = "opencode" ]; then
  addr="${MARS_LISTEN_ADDR:-0.0.0.0:4096}"
  # 监听地址可能是 0.0.0.0/[::]，容器内访问统一走回环。
  url="http://127.0.0.1:${addr##*:}"

  # 健康探测加短重试：server 活着但忙时，单次 1s 超时会把它误判为不可达，
  # 进而回退完整 TUI、起第二份 server。3 次 × 1s，最坏约 5s。
  server_ready() {
    i=1
    while [ "$i" -le 3 ]; do
      # -f：HTTP 非 2xx 也算失败，避免 /health 返回 500 时被误判为就绪。
      if curl -sfS -o /dev/null --max-time 1 "${url}/health" 2>/dev/null; then
        return 0
      fi
      if [ "$i" -lt 3 ]; then
        sleep 1
      fi
      i=$((i + 1))
    done
    return 1
  }

  # 是否已有可继续的历史会话。opencode 1.19.2 的 --continue 在「无会话可继续」时
  # 会把占位 sessionID "dummy" 当作 TUI 初始路由直接发给服务端，服务端按 ses
  # 前缀校验失败返回 500，启动页就是 "Unexpected server error. Check server logs
  # for details."。因此先探测：没有会话就不带 -c，首次进沙箱不再必现报错。
  # 注意 jq -e 对「零输入」返回 0，必须先判空，否则探测失败会被当成有会话。
  has_session_via_server() {
    body="$(curl -sfS -G --max-time 2 --data-urlencode "directory=$PWD" \
      --data-urlencode "limit=1" "${url}/session" 2>/dev/null)" || return 1
    [ -n "$body" ] || return 1
    printf '%s' "$body" |
      jq -e 'type == "array" and length > 0' >/dev/null 2>&1
  }

  if [ "$#" -eq 0 ]; then
    if server_ready; then
      # 进入 TUI 前确保 MCP 已就绪：connect API 只发起连接、不等待完成，
      # 发一次请求就 attach 会让 TUI 在 MCP 未就绪时启动（详见 mcp-wait.sh）。
      # 这里是最后一道兜底，覆盖 entrypoint 预热失败、非 /workspace 目录、
      # 常驻 server 重启等情况；等待上限可用 MARS_MCP_WAIT_TIMEOUT 调整
      # （秒，0 表示不等待）。超时只告警、不阻断：mars 网关整体故障时，
      # 用户仍应能进入 TUI 写代码。
      mcp_wait="${MARS_MCP_WAIT_TIMEOUT:-15}"
      mcp_wait_bin="${MARS_MCP_WAIT_BIN:-/usr/local/bin/mars-mcp-wait}"
      if [ -x "$mcp_wait_bin" ] && [ "$mcp_wait" -gt 0 ] 2>/dev/null; then
        "$mcp_wait_bin" --dir "$PWD" --timeout "$mcp_wait" || true
      fi
      # --dir 保持与直接启动一致：会话以用户当前目录为项目上下文。
      # 有历史会话才 -c 继续；attach 在钉版 opencode（1.19.2）不支持 --auto，
      # 自动批准由 wrapper 生成的 permission allow 规则等价实现（见 config.go）。
      if has_session_via_server; then
        exec "$bin" attach --dir "$PWD" -c "$url"
      fi
      exec "$bin" attach --dir "$PWD" "$url"
    fi
    # 常驻 server 不可达时退回完整 TUI：同样只在有历史会话时继续，并自动批准。
    # 这里问不到常驻 server，改用 opencode 自己的会话列表探测（读同一个本地库）。
    # 注意 session list 是全局的、不按目录过滤，必须自己比对 directory：
    # 别的目录（如 /workspace 下的其他项目）有会话不算「当前目录可继续」，
    # 否则仍会把 -c 带进无会话目录，复现 dummy 500。
    sessions="$("$bin" session list -n 100 --format json 2>/dev/null || true)"
    if [ -n "$sessions" ] && printf '%s' "$sessions" |
      jq -e --arg d "$PWD" 'type == "array" and any(.[]; .directory == $d)' >/dev/null 2>&1; then
      exec "$bin" -c --auto
    fi
    exec "$bin" --auto
  fi

  # `mai run`：常驻 server 可达时用 --attach 复用，避免在 CLI 进程内
  # 另起一份 server 与 MCP 连接。用户自带 --attach、文件附件或 help 时
  # 保持原样执行（attach 模式对 -f 的目录/大文件支持不同）。
  if [ "$1" = "run" ]; then
    reuse=1
    for arg in "$@"; do
      case "$arg" in
        --attach | --attach=* | -f | --file | --file=* | -h | --help | -v | --version) reuse=0 ;;
      esac
    done
    if [ "$reuse" -eq 1 ] && server_ready; then
      shift
      exec "$bin" run --attach "$url" --dir "$PWD" "$@"
    fi
  fi
fi

exec "$bin" "$@"
