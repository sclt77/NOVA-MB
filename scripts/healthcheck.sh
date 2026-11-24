#!/usr/bin/env bash
#
# 健康检查与状态汇总
#
# 用法：
#   healthcheck.sh          完整检查，有问题返回非 0
#   healthcheck.sh --json   输出 JSON（给面板调用）
#   healthcheck.sh --watch  持续监控，每 30 秒刷新
#
# 检查项：服务状态、HTTP 健康接口、数据库连通、磁盘水位、备份时效
#
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ -d "${SCRIPT_DIR}/../lib" ]]; then
    LIB_DIR="$(cd "${SCRIPT_DIR}/../lib" && pwd)"
elif [[ -d "/opt/nova/panel/lib" ]]; then
    LIB_DIR="/opt/nova/panel/lib"
else
    LIB_DIR=""
fi

if [[ -n "$LIB_DIR" ]]; then
    # shellcheck source=../lib/common.sh
    . "${LIB_DIR}/common.sh"
else
    # 没 lib 也要能跑，用最简替代
    info() { echo "[INFO] $*"; }
    ok()   { echo "[OK] $*"; }
    warn() { echo "[WARN] $*"; }
    err()  { echo "[ERROR] $*"; }
    step() { echo; echo "▸ $*"; }
fi

OUTPUT_JSON=0
WATCH=0

API_URL="http://127.0.0.1:8080/api/health"
BACKUP_DIR="/var/lib/nova/backup"
UPLOAD_DIR="/var/lib/nova/uploads"

# 阈值
DISK_WARN=80
DISK_CRIT=90
BACKUP_STALE_HOURS=48

FAILED=0

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --json)  OUTPUT_JSON=1; shift ;;
            --watch) WATCH=1; shift ;;
            -h|--help)
                sed -n '2,12p' "$0" | sed 's/^# \?//'
                exit 0
                ;;
            *) shift ;;
        esac
    done
}

# ---------------------------------------------------------------- 检查项

check_service() {
    local unit="$1" label="$2"
    local state
    state="$(systemctl is-active "$unit" 2>/dev/null || echo "unknown")"

    if [[ "$state" == "active" ]]; then
        ok "$label 运行中"
        return 0
    fi

    err "$label 未运行（$state）"
    FAILED=$((FAILED + 1))
    return 1
}

check_api() {
    local code
    code="$(curl -fsS -o /dev/null -w '%{http_code}' \
            --connect-timeout 3 --max-time 8 "$API_URL" 2>/dev/null || echo "000")"

    if [[ "$code" == "200" ]]; then
        ok "API 健康接口正常"
        return 0
    fi

    err "API 健康接口异常（HTTP $code）"
    FAILED=$((FAILED + 1))
    return 1
}

check_database() {
    if mysqladmin ping >/dev/null 2>&1; then
        ok "数据库连接正常"
        return 0
    fi

    err "数据库无法连接"
    FAILED=$((FAILED + 1))
    return 1
}

# 磁盘水位。这是最常见的线上事故来源——
# Nova 的图片不压缩，磁盘涨得比一般项目快。
check_disk() {
    local usage
    usage="$(df -P /var 2>/dev/null | awk 'NR==2 {gsub(/%/,"",$5); print $5}')"
    usage="${usage:-0}"

    local avail
    avail="$(df -Ph /var 2>/dev/null | awk 'NR==2 {print $4}')"

    if (( usage >= DISK_CRIT )); then
        err "磁盘使用率 ${usage}%（剩余 ${avail}）—— 已超过 ${DISK_CRIT}%，需要立即清理"
        FAILED=$((FAILED + 1))
        return 1
    elif (( usage >= DISK_WARN )); then
        warn "磁盘使用率 ${usage}%（剩余 ${avail}）—— 超过 ${DISK_WARN}% 告警线"
        return 0
    fi

    ok "磁盘使用率 ${usage}%（剩余 ${avail}）"
    return 0
}

# 备份时效。太久没备份等于没有备份。
check_backup() {
    if [[ ! -d "$BACKUP_DIR" ]]; then
        warn "备份目录不存在"
        return 0
    fi

    local latest
    latest="$(find "$BACKUP_DIR" -maxdepth 1 -name 'db-*.sql.gz' -printf '%T@\n' 2>/dev/null \
        | sort -rn | head -1)"

    if [[ -z "$latest" ]]; then
        warn "还没有任何数据库备份"
        return 0
    fi

    local age_hours
    age_hours=$(( ($(date +%s) - ${latest%.*}) / 3600 ))

    if (( age_hours > BACKUP_STALE_HOURS )); then
        err "最近一次备份在 ${age_hours} 小时前，超过 ${BACKUP_STALE_HOURS} 小时未备份"
        FAILED=$((FAILED + 1))
        return 1
    fi

    ok "最近备份在 ${age_hours} 小时前"
    return 0
}

check_uploads() {
    if [[ ! -d "$UPLOAD_DIR" ]]; then
        warn "上传目录不存在"
        return 0
    fi

    if [[ ! -w "$UPLOAD_DIR" ]]; then
        err "上传目录不可写，用户上传会失败"
        FAILED=$((FAILED + 1))
        return 1
    fi

    local count size
    count="$(find "$UPLOAD_DIR" -type f 2>/dev/null | wc -l)"
    size="$(du -sh "$UPLOAD_DIR" 2>/dev/null | cut -f1)"
    ok "上传目录正常（${count} 个文件，${size}）"
    return 0
}

# 端口监听检查：3306 和 8080 不该对外。
# 数据库暴露到公网是严重问题，必须检查。
check_ports() {
    local exposed=""

    if command -v ss >/dev/null 2>&1; then
        # 3306 只应该在 127.0.0.1 上
        if ss -tlnH 2>/dev/null | grep -qE '0\.0\.0\.0:3306|\*:3306|\[::\]:3306'; then
            exposed="${exposed} 3306"
        fi
        if ss -tlnH 2>/dev/null | grep -qE '0\.0\.0\.0:8080|\*:8080|\[::\]:8080'; then
            exposed="${exposed} 8080"
        fi
    fi

    if [[ -n "$exposed" ]]; then
        err "以下端口对公网开放，存在安全风险：${exposed}"
        err "  3306/8080 应只监听 127.0.0.1"
        FAILED=$((FAILED + 1))
        return 1
    fi

    ok "关键端口未对外暴露"
    return 0
}

# ---------------------------------------------------------------- 输出

run_all() {
    FAILED=0

    if [[ "$OUTPUT_JSON" == "1" ]]; then
        # JSON 模式：静默收集，最后一次性输出
        {
            local api_ok=false db_ok=false
            curl -fsS -o /dev/null --connect-timeout 3 --max-time 5 "$API_URL" 2>/dev/null \
                && api_ok=true
            mysqladmin ping >/dev/null 2>&1 && db_ok=true

            local disk_usage
            disk_usage="$(df -P /var 2>/dev/null | awk 'NR==2 {gsub(/%/,"",$5); print $5}')"

            printf '{"api":%s,"database":%s,"nova_api":"%s","nginx":"%s","mysql":"%s","disk_usage":%s}\n' \
                "$api_ok" \
                "$db_ok" \
                "$(systemctl is-active nova-api 2>/dev/null || echo unknown)" \
                "$(systemctl is-active nginx 2>/dev/null || echo unknown)" \
                "$(systemctl is-active mysql 2>/dev/null || systemctl is-active mysqld 2>/dev/null || echo unknown)" \
                "${disk_usage:-0}"
        }
        return 0
    fi

    step "服务状态"
    check_service nova-api "Nova 后端"
    check_service nginx "Nginx"

    local mysql_unit="mysql"
    systemctl list-unit-files 2>/dev/null | grep -q '^mysqld\.service' && mysql_unit="mysqld"
    check_service "$mysql_unit" "MySQL"

    step "接口与数据库"
    check_api
    check_database

    step "存储"
    check_disk
    check_uploads

    step "备份"
    check_backup

    step "安全"
    check_ports

    printf '\n'
    if (( FAILED == 0 )); then
        ok "全部检查通过"
        return 0
    else
        err "$FAILED 项检查未通过"
        return 1
    fi
}

main() {
    parse_args "$@"

    if [[ "$WATCH" == "1" ]]; then
        while true; do
            clear
            echo "Nova 健康监控  $(date '+%Y-%m-%d %H:%M:%S')  （Ctrl+C 退出）"
            run_all || true
            sleep 30
        done
    fi

    run_all
}

main "$@"
