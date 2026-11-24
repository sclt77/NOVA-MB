#!/usr/bin/env bash
# 基础依赖：包管理器封装 + 必需工具。
#
# 后面的 java.sh / mysql.sh / nginx.sh 都依赖这里的 pkg_install。

# ---------------------------------------------------------------- 包管理器

pkg_update() {
    case "$OS_FAMILY" in
        debian)
            # DEBIAN_FRONTEND 避免装包时弹交互框把脚本卡住
            DEBIAN_FRONTEND=noninteractive apt-get update -qq || {
                warn "apt-get update 失败，尝试更换软件源"
                return 1
            }
            ;;
        rhel)
            dnf makecache -q 2>/dev/null || yum makecache -q 2>/dev/null || true
            ;;
    esac
}

# 装包。已安装的会跳过（apt/dnf 自身行为），所以可以放心重复调用。
pkg_install() {
    [[ $# -eq 0 ]] && return 0

    case "$OS_FAMILY" in
        debian)
            DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
                -o Dpkg::Options::="--force-confold" \
                -o Dpkg::Options::="--force-confdef" \
                "$@" >/dev/null 2>&1
            ;;
        rhel)
            if command -v dnf >/dev/null 2>&1; then
                dnf install -y -q "$@" >/dev/null 2>&1
            else
                yum install -y -q "$@" >/dev/null 2>&1
            fi
            ;;
    esac
}

# 判断包是否已安装
pkg_installed() {
    local pkg="$1"
    case "$OS_FAMILY" in
        debian)
            dpkg -s "$pkg" >/dev/null 2>&1
            ;;
        rhel)
            rpm -q "$pkg" >/dev/null 2>&1
            ;;
    esac
}

install_deps() {
    step "安装基础依赖"

    # 先确保包管理器可用
    if [[ "$OS_FAMILY" == "debian" ]]; then
        require_cmd apt-get "这不是 Debian 系系统"
    else
        if ! command -v dnf >/dev/null 2>&1 && ! command -v yum >/dev/null 2>&1; then
            die "找不到 dnf 或 yum"
        fi
    fi

    pkg_update || warn "软件源更新失败，继续尝试安装（可能用到缓存）"

    local pkgs=(curl wget ca-certificates tar gzip unzip)
    # cron 在两个系里包名不同
    if [[ "$OS_FAMILY" == "debian" ]]; then
        pkgs+=(cron gnupg lsb-release apt-transport-https)
    else
        pkgs+=(cronie gnupg2)
    fi

    if ! pkg_install "${pkgs[@]}"; then
        die "基础依赖安装失败。请检查网络与软件源：
  Debian/Ubuntu: cat /etc/apt/sources.list
  RHEL 系:       cat /etc/yum.repos.d/*.repo"
    fi

    # 逐个确认，避免 pkg_install 静默失败
    for c in curl tar gzip unzip; do
        require_cmd "$c" "基础依赖安装不完整，请手动安装 $c 后重试"
    done

    # 确保 cron 服务在跑（备份、证书续期都靠它）
    if systemctl is-enabled cron >/dev/null 2>&1 || systemctl is-enabled crond >/dev/null 2>&1; then
        systemctl enable --now cron 2>/dev/null || systemctl enable --now crond 2>/dev/null || true
    fi

    ok "基础依赖就绪"
}
