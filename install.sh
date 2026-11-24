#!/usr/bin/env bash
#
# Nova 一键安装脚本
#
# 用法：
#   curl -sSO https://raw.githubusercontent.com/sclt77/NOVA-MB/main/install.sh
#   sudo bash install.sh
#
# 可选参数：
#   --panel-port PORT     面板端口，默认 8888
#   --domain DOMAIN       直接绑定域名，默认 _
#   --db-password PASS    指定数据库密码，默认随机生成
#   --panel-url URL       面板二进制下载地址
#   --skip-firewall       不配置防火墙
#   --uninstall           卸载（保留数据）
#
# 脚本是幂等的：重复执行不会破坏已有环境。
#
set -Eeuo pipefail

# 出错时打印出错的命令和行号，方便定位
trap 'err "第 $LINENO 行执行失败：$BASH_COMMAND"' ERR

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB_DIR="${SCRIPT_DIR}/lib"

# 如果是从 curl 管道执行，lib 目录不存在，需要先下载
if [[ ! -d "$LIB_DIR" ]]; then
    echo "检测到单文件执行模式，正在下载依赖模块..."
    BASE_URL="${NOVA_BASE_URL:-https://raw.githubusercontent.com/sclt77/NOVA-MB/main}"
    TMP_LIB="$(mktemp -d)"
    trap 'rm -rf "$TMP_LIB"' EXIT

    for mod in common detect deps java mysql nginx panel firewall summary; do
        if ! curl -fsSL --connect-timeout 15 -o "${TMP_LIB}/${mod}.sh" \
             "${BASE_URL}/lib/${mod}.sh"; then
            echo "下载 lib/${mod}.sh 失败" >&2
            exit 1
        fi
    done
    LIB_DIR="$TMP_LIB"
fi

# ---------------------------------------------------------------- 加载模块

# shellcheck source=lib/common.sh
. "${LIB_DIR}/common.sh"

# ---------------------------------------------------------------- 参数解析

NOVA_PANEL_PORT="${NOVA_PANEL_PORT:-8888}"
NOVA_DOMAIN="${NOVA_DOMAIN:-_}"
SKIP_FIREWALL=0
DO_UNINSTALL=0

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --panel-port)
                NOVA_PANEL_PORT="${2:?--panel-port 需要参数}"
                shift 2
                ;;
            --domain)
                NOVA_DOMAIN="${2:?--domain 需要参数}"
                shift 2
                ;;
            --db-password)
                NOVA_DB_PASSWORD="${2:?--db-password 需要参数}"
                shift 2
                ;;
            --panel-url)
                NOVA_PANEL_BIN_URL="${2:?--panel-url 需要参数}"
                shift 2
                ;;
            --skip-firewall)
                SKIP_FIREWALL=1
                shift
                ;;
            --uninstall)
                DO_UNINSTALL=1
                shift
                ;;
            -h|--help)
                sed -n '2,25p' "$0" | sed 's/^# \?//'
                exit 0
                ;;
            *)
                die "未知参数：$1（用 --help 查看用法）"
                ;;
        esac
    done

    # 端口合法性
    if ! [[ "$NOVA_PANEL_PORT" =~ ^[0-9]+$ ]] || \
       (( NOVA_PANEL_PORT < 1024 || NOVA_PANEL_PORT > 65535 )); then
        die "面板端口必须是 1024-65535 之间的数字，当前：$NOVA_PANEL_PORT"
    fi
}

# ---------------------------------------------------------------- 卸载

do_uninstall() {
    require_root
    step "卸载 Nova"

    warn "将停止并删除以下服务：nova-api、nova-panel"
    warn "数据目录 /var/lib/nova（含上传图片与备份）会**保留**"

    if ! confirm "确认卸载？" n; then
        info "已取消"
        exit 0
    fi

    for unit in nova-panel nova-api; do
        if systemctl list-unit-files 2>/dev/null | grep -q "^${unit}.service"; then
            systemctl stop "$unit" 2>/dev/null || true
            systemctl disable "$unit" 2>/dev/null || true
            rm -f "/etc/systemd/system/${unit}.service"
            ok "已移除 $unit"
        fi
    done

    systemctl daemon-reload

    rm -f /etc/nginx/sites-enabled/nova.conf \
          /etc/nginx/sites-available/nova.conf \
          /etc/nginx/conf.d/nova.conf 2>/dev/null || true
    systemctl reload nginx 2>/dev/null || true

    rm -f /etc/sudoers.d/nova-panel

    # 程序删掉，数据留着
    rm -rf /opt/nova/panel /opt/nova/app
    # /etc/nova 里有数据库密码，用户可能还要用来手工恢复，先留着
    warn "配置保留在 /etc/nova/，数据保留在 /var/lib/nova/"
    warn "确认不再需要后可手动删除：rm -rf /etc/nova /var/lib/nova /opt/nova"

    ok "卸载完成"
    exit 0
}

# fetch_scripts 单文件模式下把运维脚本下载到临时目录。
#
# curl | bash 时本地没有 scripts/ 目录，但这些脚本是面板执行动作的实体，
# 必须落到 /opt/nova/panel/scripts 才能工作。
fetch_scripts() {
    local base="${NOVA_BASE_URL:-https://raw.githubusercontent.com/sclt77/NOVA-MB/main}"
    local dir
    dir="$(mktemp -d)"

    info "下载运维脚本" >&2

    local list=(deploy.sh backup.sh restore.sh healthcheck.sh cron.sh)
    for name in "${list[@]}"; do
        if ! curl -fsSL --connect-timeout 15 -o "${dir}/${name}" "${base}/scripts/${name}"; then
            die "下载 scripts/${name} 失败"
        fi
        chmod 755 "${dir}/${name}"
    done

    echo "$dir"
}

# ---------------------------------------------------------------- 主流程

main() {
    # 先解析参数再检查 root：--help 应该谁都能看，
    # 否则非 root 用户连用法都读不到。
    parse_args "$@"

    require_root

    [[ "$DO_UNINSTALL" == "1" ]] && do_uninstall

    printf '\n%s%s\n' "$C_BOLD" "$C_CYAN"
    printf '  ╔══════════════════════════════════════════╗\n'
    printf '  ║        Nova 部署面板 安装程序            ║\n'
    printf '  ╚══════════════════════════════════════════╝\n'
    printf '%s\n' "$C_RESET"

    info "安装日志：$LOG_FILE"

    # 1. 环境识别
    # shellcheck source=lib/detect.sh
    . "${LIB_DIR}/detect.sh"

    detect_os
    detect_arch
    detect_init
    detect_resources

    # 端口冲突预检：装完才发现端口被占，白等半天
    if port_in_use "$NOVA_PANEL_PORT"; then
        die "端口 $NOVA_PANEL_PORT 已被占用。用 --panel-port 指定其他端口。"
    fi

    # 2. 依赖
    # shellcheck source=lib/deps.sh
    . "${LIB_DIR}/deps.sh"
    install_deps

    # 3. 运行时环境
    # shellcheck source=lib/java.sh
    . "${LIB_DIR}/java.sh"
    install_java

    # shellcheck source=lib/mysql.sh
    . "${LIB_DIR}/mysql.sh"
    install_mysql

    # shellcheck source=lib/nginx.sh
    . "${LIB_DIR}/nginx.sh"
    install_nginx

    # 4. 面板与业务
    # shellcheck source=lib/panel.sh
    . "${LIB_DIR}/panel.sh"

    # 运维脚本来源：本地目录优先，单文件模式下从仓库下载
    if [[ -z "${NOVA_SCRIPTS_DIR:-}" ]]; then
        if [[ -d "${SCRIPT_DIR}/scripts" ]]; then
            NOVA_SCRIPTS_DIR="${SCRIPT_DIR}/scripts"
        else
            NOVA_SCRIPTS_DIR="$(fetch_scripts)"
        fi
    fi
    create_users
    create_dirs
    write_env_file
    setup_sudoers
    install_panel_binary
    install_scripts
    write_systemd_units
    setup_logrotate

    # 定时任务：备份、验证、健康检查。不装的话备份永远不会自动跑。
    if [[ -x "${PANEL_DIR}/scripts/cron.sh" ]]; then
        "${PANEL_DIR}/scripts/cron.sh" install || warn "定时任务安装失败，可稍后手动执行"
    else
        warn "cron.sh 未安装，跳过定时任务配置"
    fi

    # 5. Nginx 站点（此时 uploads 目录已建好）
    write_nginx_site "$NOVA_DOMAIN" "8080"

    # 6. 防火墙
    # shellcheck source=lib/firewall.sh
    . "${LIB_DIR}/firewall.sh"
    if [[ "$SKIP_FIREWALL" == "1" ]]; then
        warn "按参数跳过防火墙配置"
    else
        setup_firewall
    fi

    # 7. 启动面板
    if [[ -x "$PANEL_DIR/nova-panel" ]]; then
        systemctl restart nova-panel 2>/dev/null || true
        sleep 2
    fi

    # 8. 汇总
    PUBLIC_IP="$(detect_public_ip)"
    # shellcheck source=lib/summary.sh
    . "${LIB_DIR}/summary.sh"
    print_summary

    ok "安装完成"
}

main "$@"
