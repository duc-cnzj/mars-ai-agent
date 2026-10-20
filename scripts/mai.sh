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
# 只在无参数、底层是 opencode、且常驻 server 可达时启用；其余情况（mai run、
# 换成其他内核、本地调试没有 serve）原样启动，行为不变。
if [ "$#" -eq 0 ] && [ "$(basename "$bin")" = "opencode" ]; then
  addr="${MARS_LISTEN_ADDR:-0.0.0.0:4096}"
  # 监听地址可能是 0.0.0.0/[::]，容器内访问统一走回环。
  url="http://127.0.0.1:${addr##*:}"
  if curl -sS -o /dev/null --max-time 1 "${url}/health" 2>/dev/null; then
    # --dir 保持与直接启动一致：会话以用户当前目录为项目上下文。
    exec "$bin" attach --dir "$PWD" "$url"
  fi
fi

exec "$bin" "$@"
