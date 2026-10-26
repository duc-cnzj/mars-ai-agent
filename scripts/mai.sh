#!/bin/sh
# mai：容器内面向用户的稳定 AI agent 入口。
#
# 用户统一用 `mai`（交互式 TUI）或 `mai run "..."`（非交互），
# 不直接依赖底层实现。内核由 MARS_AGENT_BACKEND 选择（默认 opencode），
# 全部内核相关逻辑（attach/会话探测/MCP 预热/参数透传）都在 mars-agent 内，
# 本脚本只做稳定入口，换内核时用户命令与文档都不用动。
set -eu

# MARS_AGENT_BIN 仅供本地测试/调试替换被 exec 的二进制，生产镜像默认无需设置。
exec "${MARS_AGENT_BIN:-/usr/local/bin/mars-agent}" mai "$@"
