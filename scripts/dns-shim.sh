#!/bin/sh
# host / nslookup 兼容层：镜像用 knot-dnsutils 替代 bind9 系 dnsutils
# （后者经 libxml2 拖入 30MB 的 libicu 数据），而 knot 只提供 kdig / knsupdate。
# 本脚本把 `NAME [SERVER]`（以及 host 的 `-t TYPE`）翻译成 kdig 语法，
# 复杂查询请直接用 dig（dig 是 kdig 的软链）或 kdig。
set -eu

prog="$(basename "$0")"
usage() {
    echo "usage: $prog [-t TYPE] NAME [SERVER]" >&2
    exit 1
}

type=""
while [ $# -gt 0 ]; do
    case "$1" in
        -t)
            [ $# -ge 2 ] || usage
            type="$2"
            shift 2
            ;;
        -*)
            shift
            ;;
        *)
            break
            ;;
    esac
done

[ $# -ge 1 ] || usage
name="$1"
server="${2:-}"

# kdig 语法：kdig [options] [@server] NAME
if [ -n "$type" ]; then
    set -- -t "$type"
else
    set --
fi

# 纯 IPv4 走反解，其余按名字查询。
case "$name" in
    *[!0-9.]* | '') set -- "$@" "$name" ;;
    *) set -- "$@" -x "$name" ;;
esac

if [ -n "$server" ]; then
    set -- "@$server" "$@"
fi

exec /usr/bin/kdig "$@"
