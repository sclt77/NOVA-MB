#!/usr/bin/env bash
# 安装结果汇总。
#
# 把访问地址、账号密码、后续步骤一次打印清楚。
# 这段输出用户会截图保存，所以排版要能看懂。

print_summary() {
    local public_ip="${PUBLIC_IP:-127.0.0.1}"
    local panel_port="${NOVA_PANEL_PORT:-8888}"
    local panel_user="${NOVA_PANEL_ADMIN_USER:-admin}"
    local panel_pass="${NOVA_PANEL_ADMIN_PASS:-（未生成）}"

    local line="════════════════════════════════════════════════════"
    local thin="────────────────────────────────────────────────────"

    printf '\n%s\n' "$line"
    printf '  %sNova 部署面板 安装完成%s\n' "$C_BOLD" "$C_RESET"
    printf '%s\n' "$line"
    printf '\n'

    # ---- 面板 ----
    printf '  %s面板访问%s\n' "$C_BOLD" "$C_RESET"
    printf '    http://%s:%s\n' "$public_ip" "$panel_port"
    printf '    用户名：%s\n' "$panel_user"
    printf '    密码：  %s\n' "$panel_pass"
    printf '\n'
    printf '    %s首次登录后请立即修改密码。%s\n' "$C_YELLOW" "$C_RESET"
    printf '    %s面板端口未对公网开放时，请先做 SSH 端口转发：%s\n' "$C_YELLOW" "$C_RESET"
    printf '      ssh -L %s:127.0.0.1:%s root@%s\n' "$panel_port" "$panel_port" "$public_ip"
    printf '\n'

    # ---- 业务 ----
    printf '%s\n' "$thin"
    printf '  %s服务状态%s\n' "$C_BOLD" "$C_RESET"
    print_service_state "nova-api"  "Nova 后端"
    print_service_state "nginx"     "Nginx"
    print_service_state "mysql"     "MySQL"
    print_service_state "nova-panel" "面板"
    printf '\n'

    # ---- 路径 ----
    printf '%s\n' "$thin"
    printf '  %s关键路径%s\n' "$C_BOLD" "$C_RESET"
    printf '    应用目录：  /opt/nova/app\n'
    printf '    上传文件：  /var/lib/nova/uploads\n'
    printf '    备份目录：  /var/lib/nova/backup\n'
    printf '    配置：      /etc/nova/env\n'
    printf '    日志：      /var/log/nova/\n'
    printf '\n'

    # ---- 下一步 ----
    printf '%s\n' "$thin"
    printf '  %s下一步%s\n' "$C_BOLD" "$C_RESET"
    printf '    1. 部署 Nova 业务包（面板里上传，或执行）\n'
    printf '       sudo /opt/nova/panel/scripts/deploy.sh /path/to/package.zip\n'
    printf '    2. 绑定域名（面板 → 域名管理）\n'
    printf '    3. 申请 SSL 证书（域名解析生效后）\n'
    printf '    4. 配置备份（面板 → 备份管理，建议开启每日备份 + 异地同步）\n'
    printf '\n'

    # ---- 重要提示 ----
    printf '%s\n' "$thin"
    printf '  %s请务必记住%s\n' "$C_BOLD" "$C_RESET"
    printf '    数据库密码已写入 /etc/nova/env，备份这个文件。\n'
    printf '    丢失后需要重设数据库密码才能恢复连接。\n'
    printf '\n'
    printf '%s\n' "$line"
    printf '\n'

    # 同时写一份到文件，方便以后查看
    {
        echo "Nova 安装信息 - $(date '+%Y-%m-%d %H:%M:%S')"
        echo "面板地址: http://${public_ip}:${panel_port}"
        echo "面板用户: ${panel_user}"
        echo "安装日志: ${LOG_FILE}"
    } >/var/log/nova/install-summary.txt 2>/dev/null || true
}

print_service_state() {
    local unit="$1" label="$2"
    local state
    state="$(systemctl is-active "$unit" 2>/dev/null || echo "未安装")"

    local mark color
    case "$state" in
        active)
            mark="●" color="$C_GREEN"
            ;;
        inactive|未安装)
            mark="○" color="$C_YELLOW"
            ;;
        failed)
            mark="✕" color="$C_RED"
            ;;
        *)
            mark="?" color="$C_YELLOW"
            ;;
    esac

    printf '    %s%s%s %-12s %s\n' "$color" "$mark" "$C_RESET" "$label" "$state"
}
