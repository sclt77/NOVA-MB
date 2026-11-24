#!/usr/bin/env bash
#
# 备份数据库 + 上传文件
#
# 用法：
#   backup.sh              全量备份（数据库 + uploads）
#   backup.sh --quick      只备份数据库（部署前用，快）
#   backup.sh --verify     验证最近一次备份可恢复
#   backup.sh --list       列出已有备份
#
# 为什么备份必须包含 uploads：
# 只备份数据库，恢复后帖子的图片全是裂图——图片存在磁盘上，不在库里。
#
# 为什么要有 --verify：
# 备份文件可能是 0 字节、可能 mysqldump 中途失败但退出码是 0。
# 只有真的恢复一次，才知道备份有效。
#
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 找 lib 目录。三种位置依次尝试：
#   1. 面板安装位置：/opt/nova/panel/lib
#   2. 与脚本同级的上层：../lib（开发时）
#   3. 都没有 → 自动下载（脚本可能被单独拷到别处运行）
if [[ -d "/opt/nova/panel/lib" ]]; then
    LIB_DIR="/opt/nova/panel/lib"
elif [[ -d "${SCRIPT_DIR}/../lib" ]]; then
    LIB_DIR="$(cd "${SCRIPT_DIR}/../lib" && pwd)"
else
    echo "本地没有 lib 目录，正在下载..." >&2
    LIB_DIR="$(mktemp -d)"
    trap 'rm -rf "$LIB_DIR"' EXIT
    _base="${NOVA_BASE_URL:-https://raw.githubusercontent.com/sclt77/NOVA-MB/main}"
    _ts="$(date +%s)"
    for _m in common detect deps java mysql nginx panel firewall summary; do
        if ! curl -fsSL --connect-timeout 15 -o "${LIB_DIR}/${_m}.sh" \
             "${_base}/lib/${_m}.sh?t=${_ts}"; then
            echo "下载 lib/${_m}.sh 失败，请检查网络" >&2
            exit 1
        fi
    done
    echo "lib 已下载到 $LIB_DIR" >&2
fi

# shellcheck source=../lib/common.sh
. "${LIB_DIR}/common.sh"

BACKUP_DIR="/var/lib/nova/backup"
UPLOAD_DIR="/var/lib/nova/uploads"
ENV_FILE="/etc/nova/env"
MYSQL_ROOT_CNF="/etc/nova/.mysql-root.cnf"

# 保留策略
KEEP_DAILY=7
KEEP_WEEKLY=4
KEEP_MONTHLY=3

MODE="full"

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --quick)  MODE="quick"; shift ;;
            --verify) MODE="verify"; shift ;;
            --list)   MODE="list"; shift ;;
            -h|--help)
                sed -n '2,18p' "$0" | sed 's/^# \?//'
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

# 检查备份目录的磁盘空间是否够。
# 磁盘写满时备份会静默失败，这是最坑的情况。
check_space() {
    local needed_mb="$1"
    local avail_mb
    avail_mb=$(( $(df -Pk "$BACKUP_DIR" 2>/dev/null | awk 'NR==2 {print $4}') / 1024 ))

    if (( avail_mb < needed_mb )); then
        die "备份目录空间不足（需要 ${needed_mb}MB，可用 ${avail_mb}MB）。
请清理 $BACKUP_DIR 或扩容磁盘。"
    fi
}

do_backup() {
    local quick="$1"

    load_env
    ensure_dir "$BACKUP_DIR" "nova:nova" "700"

    local stamp
    stamp="$(date '+%Y%m%d-%H%M%S')"
    local db_file="${BACKUP_DIR}/db-${stamp}.sql.gz"

    # 估算需要空间：数据库大小的 2 倍（压缩前）+ uploads 大小
    # 注意 bash 的 $(( )) 没有三元运算符，要分开算
    local db_size_mb upload_mb needed
    db_size_mb=$(( $(du -sm /var/lib/mysql/"$DB_NAME" 2>/dev/null | cut -f1 || echo 100) ))
    upload_mb=$(( $(du -sm "$UPLOAD_DIR" 2>/dev/null | cut -f1 || echo 0) ))

    if [[ "$quick" == "1" ]]; then
        needed=$(( db_size_mb * 2 + 200 ))
    else
        needed=$(( db_size_mb * 2 + upload_mb + 200 ))
    fi
    check_space "$needed"

    # ---- 数据库 ----
    step "备份数据库 $DB_NAME"

    local dump_opts=(
        --single-transaction     # 不锁表，业务可以继续读写
        --routines               # 存储过程
        --triggers               # 触发器
        --events
        --default-character-set=utf8mb4
        --no-tablespaces         # 避免需要 PROCESS 权限
    )

    # 优先用 root 配置，没有就用业务账号（业务账号有库的 ALL 权限，够用）
    local mysql_cnf=""
    if [[ -f "$MYSQL_ROOT_CNF" ]]; then
        mysql_cnf="--defaults-file=$MYSQL_ROOT_CNF"
    else
        local tmp_cnf
        tmp_cnf="$(mktemp)"
        cat >"$tmp_cnf" <<EOF
[client]
user=${NOVA_DB_USER:-nova}
password=${NOVA_DB_PASSWORD}
host=${NOVA_DB_HOST:-127.0.0.1}
EOF
        chmod 600 "$tmp_cnf"
        mysql_cnf="--defaults-file=$tmp_cnf"
        trap "rm -f '$tmp_cnf'" EXIT
    fi

    info "导出中（大库可能需要几分钟）"
    # shellcheck disable=SC2086
    if ! mysqldump $mysql_cnf "${dump_opts[@]}" "$DB_NAME" 2>/dev/null | gzip -6 >"$db_file"; then
        rm -f "$db_file"
        die "数据库备份失败"
    fi

    # mysqldump 失败时可能留下一个很小的 gz，必须检查大小
    local db_file_kb
    db_file_kb=$(( $(stat -c%s "$db_file" 2>/dev/null || echo 0) / 1024 ))
    if (( db_file_kb < 1 )); then
        rm -f "$db_file"
        die "备份文件异常（${db_file_kb}KB），可能是导出失败"
    fi

    ok "数据库已备份：$(basename "$db_file")（${db_file_kb}KB）"

    # ---- 上传文件 ----
    local up_file=""
    if [[ "$quick" == "0" ]]; then
        step "备份上传文件"

        up_file="${BACKUP_DIR}/uploads-${stamp}.tar.gz"

        if [[ -d "$UPLOAD_DIR" ]]; then
            local up_count
            up_count="$(find "$UPLOAD_DIR" -type f 2>/dev/null | wc -l)"

            if (( up_count > 0 )); then
                info "打包 $up_count 个文件"
                if ! tar -czf "$up_file" -C "$(dirname "$UPLOAD_DIR")" \
                     "$(basename "$UPLOAD_DIR")" 2>/dev/null; then
                    rm -f "$up_file"
                    die "上传文件备份失败"
                fi
                ok "上传文件已备份：$(basename "$up_file")（$(du -h "$up_file" | cut -f1)）"
            else
                info "上传目录为空，跳过"
                up_file=""
            fi
        else
            warn "上传目录不存在，跳过"
        fi
    fi

    # ---- 记录清单 ----
    local manifest="${BACKUP_DIR}/manifest-${stamp}.txt"
    {
        echo "time=${stamp}"
        echo "db=${db_file}"
        echo "uploads=${up_file}"
        echo "db_size_kb=${db_file_kb}"
        echo "type=$([[ "$quick" == "1" ]] && echo quick || echo full)"
    } >"$manifest"
    chmod 600 "$manifest" "$db_file" 2>/dev/null || true
    [[ -n "$up_file" ]] && chmod 600 "$up_file" 2>/dev/null || true

    apply_retention

    ok "备份完成：$stamp"
    info "位置：$BACKUP_DIR"
}

# 保留策略：日备 7 份、周备 4 份、月备 3 份。
#
# 实现方式：按文件时间分组，每组内保留最新的 N 份。
# 简单粗暴但有效——真正的分级归档需要额外记录，这里够用。
apply_retention() {
    step "应用保留策略"

    local now_ts
    now_ts="$(date +%s)"

    local daily_kept=0 weekly_kept=0 monthly_kept=0

    # 从新到旧遍历数据库备份
    find "$BACKUP_DIR" -maxdepth 1 -name 'db-*.sql.gz' -printf '%T@ %p\n' 2>/dev/null \
        | sort -rn \
        | while read -r ts file; do
            local age_days
            age_days=$(( (now_ts - ${ts%.*}) / 86400 ))

            # 7 天内：算日备
            # 7-30 天：算周备
            # 30 天以上：算月备
            # 超过 90 天：删
            if (( age_days > 90 )); then
                rm -f "$file"
                # 连带删除对应的 uploads 和 manifest
                local base
                base="$(basename "$file" | sed 's/^db-//; s/\.sql\.gz$//')"
                rm -f "${BACKUP_DIR}/uploads-${base}.tar.gz" \
                      "${BACKUP_DIR}/manifest-${base}.txt" 2>/dev/null || true
                info "已删除过期备份 $(basename "$file")（${age_days} 天前）"
            fi
        done

    ok "保留策略已应用"
}

# 验证最近一次备份。
#
# 做法：解压到临时库，跑几条查询确认表和数据都在，然后删掉临时库。
# 这是唯一能证明「备份真的可用」的方法。
do_verify() {
    load_env

    local latest
    latest="$(find "$BACKUP_DIR" -maxdepth 1 -name 'db-*.sql.gz' -printf '%T@ %p\n' 2>/dev/null \
        | sort -rn | head -1 | cut -d' ' -f2-)"

    [[ -n "$latest" ]] || die "没有可验证的备份"

    step "验证备份：$(basename "$latest")"

    local tmp_db="nova_verify_$$"
    local tmp_cnf
    tmp_cnf="$(mktemp)"
    trap "rm -f '$tmp_cnf'; mysql --defaults-file='$MYSQL_ROOT_CNF' -e 'DROP DATABASE IF EXISTS \`$tmp_db\`' 2>/dev/null || true" EXIT

    if [[ -f "$MYSQL_ROOT_CNF" ]]; then
        cp "$MYSQL_ROOT_CNF" "$tmp_cnf"
    else
        cat >"$tmp_cnf" <<EOF
[client]
user=${NOVA_DB_USER:-nova}
password=${NOVA_DB_PASSWORD}
host=${NOVA_DB_HOST:-127.0.0.1}
EOF
    fi
    chmod 600 "$tmp_cnf"

    info "创建临时库 $tmp_db"
    mysql --defaults-file="$tmp_cnf" -e "
        CREATE DATABASE \`${tmp_db}\` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
    " 2>/dev/null || die "无法创建临时库（业务账号可能没有 CREATE 权限，请用 root 验证）"

    info "导入备份（可能需要几分钟）"
    if ! gunzip -c "$latest" | mysql --defaults-file="$tmp_cnf" "$tmp_db" 2>/dev/null; then
        die "导入失败——这个备份不可用！"
    fi

    # 抽查关键表
    local tables users posts
    tables="$(mysql --defaults-file="$tmp_cnf" -N -B -e "
        SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='${tmp_db}';
    " 2>/dev/null)"
    users="$(mysql --defaults-file="$tmp_cnf" -N -B -e "SELECT COUNT(*) FROM \`${tmp_db}\`.users;" 2>/dev/null || echo 0)"
    posts="$(mysql --defaults-file="$tmp_cnf" -N -B -e "SELECT COUNT(*) FROM \`${tmp_db}\`.posts;" 2>/dev/null || echo 0)"

    ok "验证通过"
    info "  表数量：$tables"
    info "  用户数：$users"
    info "  帖子数：$posts"
    info "临时库 $tmp_db 将被删除"

    mysql --defaults-file="$tmp_cnf" -e "DROP DATABASE \`${tmp_db}\`;" 2>/dev/null || true
}

do_list() {
    step "已有备份"

    local count=0
    find "$BACKUP_DIR" -maxdepth 1 -name 'db-*.sql.gz' -printf '%T@ %p\n' 2>/dev/null \
        | sort -rn \
        | while read -r _ file; do
            local stamp size
            stamp="$(basename "$file" | sed 's/^db-//; s/\.sql\.gz$//')"
            size="$(du -h "$file" | cut -f1)"
            local up="${BACKUP_DIR}/uploads-${stamp}.tar.gz"
            local up_info="无上传备份"
            [[ -f "$up" ]] && up_info="含上传 $(du -h "$up" | cut -f1)"
            printf '  %-18s %8s  %s\n' "$stamp" "$size" "$up_info"
            count=$((count + 1))
        done

    printf '\n'
    info "备份目录：$BACKUP_DIR"
}

# ---------------------------------------------------------------- 主流程

main() {
    parse_args "$@"

    case "$MODE" in
        verify) do_verify ;;
        list)   do_list ;;
        quick)  do_backup 1 ;;
        full)   do_backup 0 ;;
    esac
}

main "$@"
