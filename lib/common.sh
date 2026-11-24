#!/usr/bin/env bash
# 公共函数：日志、颜色、错误处理、幂等工具。
#
# 被 install.sh 与 scripts/*.sh source。不要直接执行。

# 只在被 source 时生效，避免误执行
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    echo "这个文件是被 source 的，不要直接执行" >&2
    exit 1
fi

# ---------------------------------------------------------------- 颜色

if [[ -t 1 ]]; then
    C_RESET=$'\033[0m'
    C_RED=$'\033[31m'
    C_GREEN=$'\033[32m'
    C_YELLOW=$'\033[33m'
    C_BLUE=$'\033[34m'
    C_CYAN=$'\033[36m'
    C_BOLD=$'\033[1m'
else
    # 重定向到文件时不要塞一堆控制字符
    C_RESET='' C_RED='' C_GREEN='' C_YELLOW='' C_BLUE='' C_CYAN='' C_BOLD=''
fi

# ---------------------------------------------------------------- 日志

LOG_FILE="${NOVA_INSTALL_LOG:-/var/log/nova/install.log}"

_log() {
    local level="$1" color="$2"
    shift 2
    local msg="$*"
    local ts
    ts="$(date '+%Y-%m-%d %H:%M:%S')"

    printf '%s%s [%s]%s %s\n' "$color" "$ts" "$level" "$C_RESET" "$msg"

    # 日志目录可能还没建，静默跳过
    if [[ -n "$LOG_FILE" ]] && mkdir -p "$(dirname "$LOG_FILE")" 2>/dev/null; then
        printf '%s [%s] %s\n' "$ts" "$level" "$msg" >>"$LOG_FILE" 2>/dev/null || true
    fi
}

info()  { _log INFO  "$C_BLUE"   "$@"; }
ok()    { _log OK    "$C_GREEN"  "$@"; }
warn()  { _log WARN  "$C_YELLOW" "$@"; }
err()   { _log ERROR "$C_RED"    "$@"; }

step() {
    printf '\n%s%s▸ %s%s\n' "$C_BOLD" "$C_CYAN" "$*" "$C_RESET"
    _log STEP "$C_CYAN" "$*"
}

# 致命错误：打印并退出。安装脚本任何一步失败都应该直接中止，
# 不能"继续往下装"，否则会留下半成品环境，比装失败更难排查。
die() {
    err "$@"
    err "安装已中止。修复上面的问题后重新执行本脚本即可（脚本是幂等的）。"
    exit 1
}

# ---------------------------------------------------------------- 前置检查

require_root() {
    if [[ "${EUID}" -ne 0 ]]; then
        die "请用 root 执行：sudo bash $0"
    fi
}

require_cmd() {
    local cmd="$1" hint="${2:-}"
    if ! command -v "$cmd" >/dev/null 2>&1; then
        if [[ -n "$hint" ]]; then
            die "缺少命令 $cmd。$hint"
        else
            die "缺少命令 $cmd"
        fi
    fi
}

# ---------------------------------------------------------------- 幂等工具

# 目录不存在才创建，并设定属主
ensure_dir() {
    local dir="$1" owner="${2:-}" mode="${3:-}"
    if [[ ! -d "$dir" ]]; then
        mkdir -p "$dir" || die "无法创建目录 $dir"
    fi
    [[ -n "$owner" ]] && chown "$owner" "$dir" 2>/dev/null || true
    [[ -n "$mode" ]] && chmod "$mode" "$dir" || true
}

# 文件不存在才写入内容（stdin）。已存在则跳过，保护用户改动。
ensure_file() {
    local path="$1" owner="${2:-}" mode="${3:-}"
    if [[ -f "$path" ]]; then
        info "已存在，跳过：$path"
        return 0
    fi
    ensure_dir "$(dirname "$path")"
    cat >"$path" || die "无法写入 $path"
    [[ -n "$owner" ]] && chown "$owner" "$path" 2>/dev/null || true
    [[ -n "$mode" ]] && chmod "$mode" "$path" || true
    ok "已创建：$path"
}

# 检查某个 systemd 服务是否已存在并启用
service_enabled() {
    systemctl list-unit-files --type=service --no-legend 2>/dev/null \
        | awk '{print $1}' | grep -qx "$1.service"
}

# 生成随机密码。用 /dev/urandom 而不是 $RANDOM（后者熵太低）。
gen_password() {
    local len="${1:-16}"

    # 不能写成 `tr -dc ... </dev/urandom | head -c N`：
    # tr 会一直读到 /dev/urandom 结束（永远不结束），head 读够就关管道，
    # tr 收到 SIGPIPE 退出（141），在 `set -o pipefail` 下整条管道返回 141，
    # 再配合 `set -e` 直接把安装脚本打死。这个坑在真实服务器上才暴露。
    #
    # 改成：先取一块定长随机数据，再用 tr 过滤，最后截断。
    # 整条链路都是有限输入，不会有 SIGPIPE。
    local raw
    raw=$(dd if=/dev/urandom bs=256 count=1 2>/dev/null \
          | tr -dc 'A-HJ-NP-Za-km-z2-9')

    # 极少数情况下过滤后不足 len（概率极低），补一次
    while [ "${#raw}" -lt "$len" ]; do
        raw="${raw}$(dd if=/dev/urandom bs=256 count=1 2>/dev/null \
                      | tr -dc 'A-HJ-NP-Za-km-z2-9')"
    done

    printf '%s' "${raw:0:$len}"
}

# ---------------------------------------------------------------- 下载

# 下载文件到指定路径。带重试与超时，避免网络抖动导致安装中断。
download() {
    local url="$1" dest="$2" retries="${3:-3}"
    local attempt=1

    while (( attempt <= retries )); do
        if command -v curl >/dev/null 2>&1; then
            if curl -fsSL --connect-timeout 15 --max-time 600 -o "$dest" "$url"; then
                return 0
            fi
        elif command -v wget >/dev/null 2>&1; then
            if wget -q --timeout=15 --tries=1 -O "$dest" "$url"; then
                return 0
            fi
        else
            die "既没有 curl 也没有 wget，无法下载"
        fi
        warn "下载失败（第 $attempt/$retries 次）：$url"
        attempt=$((attempt + 1))
        sleep 2
    done

    return 1
}

# 校验 SHA256。校验不通过必须中止——宁可装不上，也不能装一个被篡改的包。
verify_sha256() {
    local file="$1" expected="$2"

    if [[ -z "$expected" ]]; then
        warn "未提供 SHA256，跳过校验（仅限开发环境）"
        return 0
    fi

    local actual
    if command -v sha256sum >/dev/null 2>&1; then
        actual="$(sha256sum "$file" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        actual="$(shasum -a 256 "$file" | awk '{print $1}')"
    else
        warn "系统没有 sha256sum/shasum，跳过校验"
        return 0
    fi

    if [[ "$actual" != "$expected" ]]; then
        err "SHA256 校验失败"
        err "  期望: $expected"
        err "  实际: $actual"
        err "文件可能被篡改或下载不完整，已中止。"
        return 1
    fi

    ok "SHA256 校验通过"
    return 0
}

# ---------------------------------------------------------------- 确认

# 交互确认。非交互环境（如管道安装）默认拒绝危险操作。
confirm() {
    local prompt="$1" default="${2:-n}"

    if [[ ! -t 0 ]]; then
        # 管道进来的时候读不到用户输入，按默认值处理
        [[ "$default" == "y" ]]
        return
    fi

    local hint="[y/N]"
    [[ "$default" == "y" ]] && hint="[Y/n]"

    local answer
    read -r -p "$prompt $hint " answer
    answer="${answer:-$default}"
    [[ "$answer" =~ ^[Yy]$ ]]
}

# ---------------------------------------------------------------- 版本比较

# 判断 $1 是否 >= $2（版本号，形如 8.0.35）
version_ge() {
    printf '%s\n%s\n' "$2" "$1" | sort -V -C && return 0 || return 1
}
