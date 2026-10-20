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

exec "${MARS_AI_BIN:-/usr/local/bin/opencode}" "$@"
