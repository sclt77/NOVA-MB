#!/usr/bin/env bash
#
# 从备份恢复
#
# 用法：
#   restore.sh                         列出可用备份并交互选择
#   restore.sh --db 20261124-103000    只恢复数据库
#   restore.sh --all 20261124-103000   恢复数据库 + 上传文件
#   restore.sh --uploads 20261124-103000  只恢复上传文件
#
# 危险操作：会覆盖现有数据。脚本会先自动备份当前状态，再恢复。
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

BACKUP_DIR="/var/lib/nova/backup"
UPLOAD_DIR="/var/lib/nova/uploads"
ENV_FILE="/etc/nova/env"
MYSQL_ROOT_CNF="/etc/nova/.mysql-root.cnf"

TARGET=""
SCOPE=""

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --db)      SCOPE="db";      TARGET="${2:?需要时间戳}"; shift 2 ;;
            --uploads) SCOPE="uploads"; TARGET="${2:?需要时间戳}"; shift 2 ;;
            --all)     SCOPE="all";     TARGET="${2:?需要时间戳}"; shift 2 ;;
            -h|--help)
                sed -n '2,15p' "$0" | sed 's/^# \?//'
                exit 0
                ;;
            *) die "未知参数：$1" ;;
        esac
    done
}

load_env() {
    [[ -f "$ENV_FILE" ]] || die "找不到 $ENV_FILE"
    # shellcheck disable=SC1090
    set -a; . "$ENV_FILE"; set +a
    DB_NAME="${NOVA_DB_NAME:-nova}"
}

mysql_cnf() {
    if [[ -f "$MYSQL_ROOT_CNF" ]]; then
        echo "--defaults-file=$MYSQL_ROOT_CNF"
    else
        local tmp
        tmp="$(mktemp)"
        cat >"$tmp" <<EOF
[client]
user=${NOVA_DB_USER:-nova}
password=${NOVA_DB_PASSWORD}
host=${NOVA_DB_HOST:-127.0.0.1}
EOF
        chmod 600 "$tmp"
        echo "--defaults-file=$tmp"
    fi
}

# 列出备份供选择
pick_backup() {
    local backups
    backups="$(find "$BACKUP_DIR" -maxdepth 1 -name 'db-*.sql.gz' -printf '%T@ %p\n' 2>/dev/null \
        | sort -rn | head -20)"

    [[ -n "$backups" ]] || die "没有可用的备份"

    step "可用备份"

    local i=1
    local stamps=()
    while read -r _ file; do
        local stamp size
        stamp="$(basename "$file" | sed 's/^db-//; s/\.sql\.gz$//')"
        size="$(du -h "$file" | cut -f1)"
        printf '  %2d) %-18s %8s\n' "$i" "$stamp" "$size"
        stamps+=("$stamp")
        i=$((i + 1))
    done <<<"$backups"

    printf '\n'
    local choice
    read -r -p "选择要恢复的备份编号（直接回车取消）: " choice

    [[ -z "$choice" ]] && { info "已取消"; exit 0; }

    if ! [[ "$choice" =~ ^[0-9]+$ ]] || (( choice < 1 || choice > ${#stamps[@]} )); then
        die "无效的编号：$choice"
    fi

    TARGET="${stamps[$((choice - 1))]}"
    SCOPE="all"
}

# 恢复前先把当前状态备份一份。
# 恢复本身也是危险操作，万一选错了备份，至少还能回来。
backup_current() {
    step "恢复前备份当前状态"

    local script="${SCRIPT_DIR}/backup.sh"
    if [[ -x "$script" ]]; then
        if "$script" --quick; then
            ok "当前状态已备份"
        else
            warn "备份失败"
            confirm "继续恢复？（当前数据将无法找回）" n || { info "已取消"; exit 0; }
        fi
    else
        warn "找不到 backup.sh，跳过"
        confirm "继续恢复？（当前数据将无法找回）" n || { info "已取消"; exit 0; }
    fi
}

do_restore_db() {
    local stamp="$1"
    local file="${BACKUP_DIR}/db-${stamp}.sql.gz"

    [[ -f "$file" ]] || die "备份不存在：$file"

    step "恢复数据库"

    # 先停业务，避免恢复过程中有写入
    info "停止 nova-api"
    systemctl stop nova-api 2>/dev/null || true

    local cnf
    cnf="$(mysql_cnf)"

    info "清空现有数据"
    # 不用 DROP DATABASE，因为业务账号可能没权限；
    # 用删表的方式，效果一样且权限要求低。
    # shellcheck disable=SC2086
    mysql $cnf -N -B -e "
        SELECT CONCAT('DROP TABLE IF EXISTS \`', table_name, '\`;')
        FROM information_schema.TABLES WHERE table_schema='${DB_NAME}';
    " 2>/dev/null | mysql $cnf "$DB_NAME" 2>/dev/null || {
        err "清空数据失败"
        systemctl start nova-api 2>/dev/null || true
        die "已恢复服务，数据未改动"
    }

    info "导入备份（可能需要几分钟）"
    if ! gunzip -c "$file" | mysql $cnf "$DB_NAME" 2>/dev/null; then
        err "导入失败！数据库现在是不完整状态。"
        err "请立即用恢复前自动生成的备份重试："
        err "  $0 --db <刚才的时间戳>"
        systemctl start nova-api 2>/dev/null || true
        die "恢复失败"
    fi

    ok "数据库已恢复"
}

do_restore_uploads() {
    local stamp="$1"
    local file="${BACKUP_DIR}/uploads-${stamp}.tar.gz"

    if [[ ! -f "$file" ]]; then
        warn "该备份不含上传文件，跳过"
        return 0
    fi

    step "恢复上传文件"

    # 先把现有 uploads 挪走，不要直接删——
    # 万一恢复的文件不全，还能手工找回。
    if [[ -d "$UPLOAD_DIR" ]]; then
        local old="${UPLOAD_DIR}.old-$(date +%s)"
        info "现有文件移到 $old"
        mv "$UPLOAD_DIR" "$old" || die "无法移动现有上传目录"
        warn "恢复成功后请手动删除：rm -rf $old"
    fi

    ensure_dir "$UPLOAD_DIR" "nova:nova" "755"

    info "解压备份"
    if ! tar -xzf "$file" -C "$(dirname "$UPLOAD_DIR")" 2>/dev/null; then
        err "解压失败"
        die "上传文件恢复失败，原有文件在 ${UPLOAD_DIR}.old-*"
    fi

    chown -R nova:nova "$UPLOAD_DIR" 2>/dev/null || true
    ok "上传文件已恢复（$(find "$UPLOAD_DIR" -type f | wc -l) 个文件）"
}

# ---------------------------------------------------------------- 主流程

main() {
    require_root
    parse_args "$@"

    load_env

    if [[ -z "$TARGET" ]]; then
        pick_backup
    fi

    # 展示将要做什么，让人确认
    printf '\n'
    warn "即将恢复备份：$TARGET"
    case "$SCOPE" in
        db)      warn "  范围：仅数据库（现有数据将被覆盖）" ;;
        uploads) warn "  范围：仅上传文件（现有图片将被替换）" ;;
        all)     warn "  范围：数据库 + 上传文件（全部覆盖）" ;;
    esac
    printf '\n'

    confirm "确认执行恢复？" n || { info "已取消"; exit 0; }

    backup_current

    case "$SCOPE" in
        db)      do_restore_db "$TARGET" ;;
        uploads) do_restore_uploads "$TARGET" ;;
        all)
            do_restore_db "$TARGET"
            do_restore_uploads "$TARGET"
            ;;
    esac

    step "重启服务"
    systemctl start nova-api 2>/dev/null || true
    sleep 3

    if systemctl is-active --quiet nova-api; then
        ok "nova-api 已启动"
    else
        err "nova-api 启动失败，请查看日志：journalctl -u nova-api -n 50"
    fi

    ok "恢复完成"
}

main "$@"
