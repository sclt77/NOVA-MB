#!/usr/bin/env bash
# 防火墙：只放行必要端口。
#
# 原则：默认拒绝入站，只开 22（SSH）、80、443（Web）和面板端口。
# 数据库 3306、后端 8080 一律不对外——它们只监听 127.0.0.1。

setup_firewall() {
    step "配置防火墙"

    local panel_port="${NOVA_PANEL_PORT:-8888}"

    # 优先用 ufw（Debian 系），其次 firewalld（RHEL 系）
    if command -v ufw >/dev/null 2>&1 || pkg_install ufw 2>/dev/null; then
        setup_ufw "$panel_port"
    elif command -v firewall-cmd >/dev/null 2>&1; then
        setup_firewalld "$panel_port"
    else
        warn "未找到 ufw 或 firewalld，跳过防火墙配置"
        warn "请手动确认：3306 与 8080 不对公网开放"
    fi
}

setup_ufw() {
    local panel_port="$1"

    # 先放行 SSH，再启用。顺序反了会把自己锁在门外。
    ufw allow 22/tcp >/dev/null 2>&1 || true
    ufw allow 80/tcp >/dev/null 2>&1 || true
    ufw allow 443/tcp >/dev/null 2>&1 || true
    ufw allow "${panel_port}/tcp" >/dev/null 2>&1 || true

    # 显式拒绝数据库端口（万一以后有人改了 bind-address）
    ufw deny 3306/tcp >/dev/null 2>&1 || true

    if ! ufw status 2>/dev/null | grep -q "Status: active"; then
        # --force 避免交互确认把脚本卡住
        ufw --force enable >/dev/null 2>&1 || {
            warn "ufw 启用失败，请手动检查"
            return
        }
    fi

    ok "ufw 已配置：22/80/443/${panel_port} 放行，3306 拒绝"
}

setup_firewalld() {
    local panel_port="$1"

    systemctl enable --now firewalld >/dev/null 2>&1 || true

    firewall-cmd --permanent --add-service=ssh >/dev/null 2>&1 || true
    firewall-cmd --permanent --add-service=http >/dev/null 2>&1 || true
    firewall-cmd --permanent --add-service=https >/dev/null 2>&1 || true
    firewall-cmd --permanent --add-port="${panel_port}/tcp" >/dev/null 2>&1 || true
    firewall-cmd --reload >/dev/null 2>&1 || true

    ok "firewalld 已配置：22/80/443/${panel_port} 放行"
}

# 检测公网 IP，用于安装完成后提示访问地址
detect_public_ip() {
    local ip=""

    # 依次尝试几个服务，任一成功即可
    for url in https://api.ipify.org https://ifconfig.me/ip https://icanhazip.com; do
        ip="$(curl -fsS --connect-timeout 5 --max-time 8 "$url" 2>/dev/null | tr -d '[:space:]')"
        if [[ "$ip" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
            echo "$ip"
            return 0
        fi
    done

    # 拿不到就退回本机第一个非回环地址
    ip="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1)}' | head -1)"
    echo "${ip:-127.0.0.1}"
}
