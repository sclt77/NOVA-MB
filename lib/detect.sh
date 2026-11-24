#!/usr/bin/env bash
# 系统识别：发行版、版本、架构、资源。
#
# 识别结果供后续模块决定用 apt 还是 dnf、装哪个 MySQL 源。
# 不支持的系统直接中止——硬装下去只会得到一堆诡异的错误。

detect_os() {
    if [[ ! -f /etc/os-release ]]; then
        die "找不到 /etc/os-release，无法识别系统。本脚本仅支持主流 Linux 发行版。"
    fi

    # shellcheck disable=SC1091
    . /etc/os-release

    OS_ID="${ID:-unknown}"
    OS_VERSION="${VERSION_ID:-0}"
    OS_NAME="${PRETTY_NAME:-$OS_ID $OS_VERSION}"

    case "$OS_ID" in
        ubuntu|debian)
            OS_FAMILY="debian"
            ;;
        centos|rhel|almalinux|rocky|fedora|openeuler|anolis)
            OS_FAMILY="rhel"
            ;;
        *)
            # 兜底：有 apt 就按 debian 处理，有 dnf/yum 就按 rhel
            if command -v apt-get >/dev/null 2>&1; then
                OS_FAMILY="debian"
                warn "未识别的发行版 $OS_ID，按 Debian 系处理"
            elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
                OS_FAMILY="rhel"
                warn "未识别的发行版 $OS_ID，按 RHEL 系处理"
            else
                die "不支持的系统：$OS_NAME。需要 apt 或 dnf/yum。"
            fi
            ;;
    esac

    # CentOS 7 的 OpenSSL 太老，装不了 MySQL 8 官方源
    if [[ "$OS_ID" == "centos" && "${OS_VERSION%%.*}" == "7" ]]; then
        die "CentOS 7 已停止维护（EOL 2024-06），且 OpenSSL 版本过低装不了 MySQL 8。
建议换用 AlmaLinux 9 / Rocky Linux 9 / Ubuntu 22.04+。"
    fi

    ok "系统：$OS_NAME（$OS_FAMILY 系）"
}

detect_arch() {
    local machine
    machine="$(uname -m)"
    case "$machine" in
        x86_64|amd64)
            ARCH="amd64"
            ;;
        aarch64|arm64)
            ARCH="arm64"
            ;;
        *)
            die "不支持的 CPU 架构：$machine。仅支持 x86_64 与 aarch64。"
            ;;
    esac
    ok "架构：$ARCH（$machine）"
}

detect_resources() {
    # 内存（MB）
    local mem_kb
    mem_kb="$(awk '/MemTotal/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
    MEM_MB=$((mem_kb / 1024))

    # 磁盘可用（MB），取 /var 所在分区
    local avail_kb
    avail_kb="$(df -Pk /var 2>/dev/null | awk 'NR==2 {print $4}' || echo 0)"
    DISK_FREE_MB=$((avail_kb / 1024))

    # CPU 核数
    CPU_CORES="$(nproc 2>/dev/null || echo 1)"

    ok "资源：${CPU_CORES} 核 / ${MEM_MB} MB 内存 / /var 可用 ${DISK_FREE_MB} MB"

    # 硬性门槛：低于这个值装完也跑不动，提前拦住比装到一半失败好
    if (( MEM_MB < 1500 )); then
        die "内存不足（${MEM_MB} MB）。Nova 后端 + MySQL 至少需要 1.5 GB。
建议 2 核 4G 起步。"
    fi

    if (( MEM_MB < 1900 )); then
        warn "内存偏小（${MEM_MB} MB），将自动调低 MySQL 缓冲池。"
        LOW_MEMORY=1
    else
        LOW_MEMORY=0
    fi

    # 磁盘：jar + 依赖 + 一年图片，留 8G 打底
    if (( DISK_FREE_MB < 8000 )); then
        warn "磁盘可用空间偏小（${DISK_FREE_MB} MB）。
Nova 的图片不压缩，100 个活跃用户约 4GB/月，建议至少预留 20GB。"
    fi
}

# 检测 systemd。没有 systemd 的容器环境（如部分 LXC）需要另做适配。
detect_init() {
    if ! command -v systemctl >/dev/null 2>&1; then
        die "找不到 systemctl。本面板依赖 systemd 管理服务，
OpenVZ/LXC 等容器环境请改用 Docker 部署方案。"
    fi
    if [[ "$(ps -p 1 -o comm= 2>/dev/null)" != "systemd" ]]; then
        die "PID 1 不是 systemd，当前环境不支持。"
    fi
    ok "初始化系统：systemd"
}

# 检查端口是否被占用。安装前发现冲突，比装完起不来好。
port_in_use() {
    local port="$1"
    if command -v ss >/dev/null 2>&1; then
        ss -tlnH 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}$"
    elif command -v netstat >/dev/null 2>&1; then
        netstat -tln 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}$"
    else
        return 1
    fi
}
