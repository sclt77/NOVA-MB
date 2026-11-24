#!/usr/bin/env bash
#
# 定时任务管理
#
# 用法：
#   cron.sh install      安装默认定时任务
#   cron.sh remove       移除所有 Nova 定时任务
#   cron.sh list         查看当前任务
#
# 只允许预定义的任务，不接受自由填写的命令。
# 这是安全边界的一部分——能任意写 crontab 等于能任意执行命令。
#
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ -d "${SCRIPT_DIR}/../lib" ]]; then
    LIB_DIR="$(cd "${SCRIPT_DIR}/../lib" && pwd)"
elif [[ -d "/opt/nova/panel/lib" ]]; then
    LIB_DIR="/opt/nova/panel/lib"
else
    echo "找不到 lib 目录" >&2
    exit 1
fi

# shellcheck source=../lib/common.sh
. "${LIB_DIR}/common.sh"

SCRIPTS_DIR="/opt/nova/panel/scripts"
LOG="/var/log/nova/cron.log"
TAG="# nova-panel-managed"

ACTION="${1:-list}"

# 备份时间可配置，默认凌晨 3 点
BACKUP_HOUR="${NOVA_BACKUP_HOUR:-3}"
BACKUP_MINUTE="${NOVA_BACKUP_MINUTE:-0}"

do_install() {
    require_root

    step "安装定时任务"

    [[ -x "${SCRIPTS_DIR}/backup.sh" ]] || \
        die "找不到 ${SCRIPTS_DIR}/backup.sh，请先部署运维脚本"

    # 先清掉旧的，避免重复添加
    remove_tagged

    # 用 root 的 crontab：备份要读 MySQL 数据目录和 /etc/nova/env
    local current
    current="$(crontab -l 2>/dev/null || true)"

    local new_tasks
    new_tasks="$(cat <<EOF
${BACKUP_MINUTE} ${BACKUP_HOUR} * * * ${SCRIPTS_DIR}/backup.sh >> ${LOG} 2>&1 ${TAG}
0 4 * * 0 ${SCRIPTS_DIR}/backup.sh --verify >> ${LOG} 2>&1 ${TAG}
*/10 * * * * ${SCRIPTS_DIR}/healthcheck.sh >> ${LOG} 2>&1 ${TAG}
EOF
)"

    if [[ -n "$current" ]]; then
        printf '%s\n%s\n' "$current" "$new_tasks" | crontab -
    else
        printf '%s\n' "$new_tasks" | crontab -
    fi

    ok "定时任务已安装："
    info "  每天 ${BACKUP_HOUR}:$(printf '%02d' "$BACKUP_MINUTE")  全量备份"
    info "  每周日 04:00      验证备份可恢复"
    info "  每 10 分钟        健康检查"

    # cron 服务必须活着，否则任务不会跑
    systemctl enable --now cron 2>/dev/null || \
        systemctl enable --now crond 2>/dev/null || \
        warn "无法启动 cron 服务，定时任务可能不会执行"
}

# 只删带标记的行，不碰用户自己加的任务
remove_tagged() {
    local current
    current="$(crontab -l 2>/dev/null || true)"

    if [[ -z "$current" ]]; then
        return 0
    fi

    local filtered
    filtered="$(printf '%s\n' "$current" | grep -vF "$TAG" || true)"

    if [[ -z "$filtered" ]]; then
        crontab -r 2>/dev/null || true
    else
        printf '%s\n' "$filtered" | crontab -
    fi
}

do_remove() {
    require_root
    step "移除 Nova 定时任务"
    remove_tagged
    ok "已移除（其他 crontab 任务保留）"
}

do_list() {
    step "当前定时任务"

    local all
    all="$(crontab -l 2>/dev/null || true)"

    if [[ -z "$all" ]]; then
        info "没有任何定时任务"
        return 0
    fi

    local mine others
    mine="$(printf '%s\n' "$all" | grep -F "$TAG" || true)"
    others="$(printf '%s\n' "$all" | grep -vF "$TAG" || true)"

    if [[ -n "$mine" ]]; then
        printf '  %sNova 任务：%s\n' "$C_BOLD" "$C_RESET"
        printf '%s\n' "$mine" | sed "s/ ${TAG}//" | sed 's/^/    /'
    else
        info "没有 Nova 的定时任务（执行 cron.sh install 安装）"
    fi

    if [[ -n "$others" ]]; then
        printf '\n  %s其他任务：%s\n' "$C_BOLD" "$C_RESET"
        printf '%s\n' "$others" | sed 's/^/    /'
    fi
}

case "$ACTION" in
    install) do_install ;;
    remove)  do_remove ;;
    list)    do_list ;;
    -h|--help)
        sed -n '2,14p' "$0" | sed 's/^# \?//'
        ;;
    *)
        die "未知操作：$ACTION（可用：install / remove / list）"
        ;;
esac
