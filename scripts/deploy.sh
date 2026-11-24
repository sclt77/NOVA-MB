#!/usr/bin/env bash
#
# 部署 Nova 业务包
#
# 用法：
#   deploy.sh /path/to/package.zip
#   deploy.sh /path/to/package.zip --skip-backup
#   deploy.sh --rollback             回滚到上一个版本
#   deploy.sh --list                 列出历史版本
#
# 流程：校验 → 解压 → 备份 → 停服 → 切换 → 启动 → 健康检查 → 清理
# 健康检查失败会自动回滚，不会把站点留在半死状态。
#
set -Eeuo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 找 lib 目录：面板里装在 scripts/ 下，开发时在 ../lib
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

APP_DIR="/opt/nova/app"
RELEASES_DIR="/opt/nova/releases"
CURRENT_LINK="/opt/nova/app/current"
KEEP_RELEASES=5

HEALTH_URL="http://127.0.0.1:8080/api/health"
HEALTH_RETRIES=30
HEALTH_INTERVAL=2

SKIP_BACKUP=0

# ---------------------------------------------------------------- 参数

PACKAGE=""
ACTION="deploy"

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --rollback) ACTION="rollback"; shift ;;
            --list)     ACTION="list"; shift ;;
            --skip-backup) SKIP_BACKUP=1; shift ;;
            -h|--help)
                sed -n '2,18p' "$0" | sed 's/^# \?//'
                exit 0
                ;;
            -*)
                die "未知参数：$1"
                ;;
            *)
                PACKAGE="$1"
                shift
                ;;
        esac
    done
}

# ---------------------------------------------------------------- 校验包

# 检查 zip 结构是否符合约定。
#
# 期望结构：
#   nova-api.jar
#   web/                 前端静态文件（index.html 必须在）
#   manifest.json        版本信息（可选，但建议有）
#
# 不做校验直接解压，等于让上传者决定往磁盘上写什么。
validate_package() {
    local pkg="$1"

    [[ -f "$pkg" ]] || die "包不存在：$pkg"
    [[ -s "$pkg" ]] || die "包是空文件：$pkg"

    # 大小上限 500MB。部署包不该这么大，超过基本是传错了。
    local size_mb
    size_mb=$(( $(stat -c%s "$pkg" 2>/dev/null || stat -f%z "$pkg") / 1024 / 1024 ))
    (( size_mb <= 500 )) || die "包过大（${size_mb}MB），上限 500MB"

    command -v unzip >/dev/null 2>&1 || die "需要 unzip，请先安装"

    # zip 完整性
    if ! unzip -t "$pkg" >/dev/null 2>&1; then
        die "压缩包已损坏，请重新上传"
    fi

    # 结构检查
    local listing
    listing="$(unzip -l "$pkg" 2>/dev/null || true)"

    if ! grep -qE '(^|/)nova-api\.jar$' <<<"$listing"; then
        die "包内缺少 nova-api.jar"
    fi
    if ! grep -qE '(^|/)web/index\.html$' <<<"$listing"; then
        die "包内缺少 web/index.html"
    fi

    # zip slip 防护。
    #
    # 用 unzip -Z1 只取文件名列表，不解析 `unzip -l` 的表格输出——
    # 那个格式里有日期、大小、空格对齐，正则容易误判或漏判。
    local names
    names="$(unzip -Z1 "$pkg" 2>/dev/null || true)"

    [[ -n "$names" ]] || die "无法读取包内文件列表"

    while IFS= read -r name; do
        [[ -z "$name" ]] && continue

        # 绝对路径：解压会写到包外的位置
        if [[ "$name" == /* ]]; then
            die "包内含绝对路径：$name"
        fi

        # 路径穿越：../../etc/passwd 这类。
        # 只认 .. 作为完整路径段，允许文件名里带两个点（如 v1..2.txt）。
        if [[ "/${name}/" == *"/../"* ]]; then
            die "包内含路径穿越：$name"
        fi
    done <<<"$names"

    ok "包校验通过（${size_mb}MB）"
}

# ---------------------------------------------------------------- 备份

backup_before_deploy() {
    [[ "$SKIP_BACKUP" == "1" ]] && { warn "按要求跳过备份"; return 0; }

    step "部署前备份数据库"

    local backup_script="${SCRIPT_DIR}/backup.sh"
    if [[ ! -x "$backup_script" ]]; then
        warn "找不到 backup.sh，跳过备份"
        warn "建议手动备份后再部署改表结构的版本"
        return 0
    fi

    if ! "$backup_script" --quick; then
        die "备份失败。部署已中止。
改表结构的版本（Flyway 迁移）没有备份就上线，出问题无法恢复。"
    fi

    ok "备份完成"
}

# ---------------------------------------------------------------- 部署

do_deploy() {
    local pkg="$1"

    validate_package "$pkg"
    backup_before_deploy

    step "准备发布目录"

    local stamp release_dir
    stamp="$(date '+%Y%m%d-%H%M%S')"
    release_dir="${RELEASES_DIR}/${stamp}"

    ensure_dir "$release_dir"

    info "解压到 $release_dir"
    if ! unzip -q -o "$pkg" -d "$release_dir"; then
        rm -rf "$release_dir"
        die "解压失败"
    fi

    # 有的打包工具会多套一层目录，把内容提到根上
    if [[ ! -f "${release_dir}/nova-api.jar" ]]; then
        local inner
        inner="$(find "$release_dir" -maxdepth 2 -name "nova-api.jar" -printf '%h\n' 2>/dev/null | head -1)"
        if [[ -n "$inner" && "$inner" != "$release_dir" ]]; then
            info "检测到嵌套目录，提升内容"
            shopt -s dotglob
            mv "$inner"/* "$release_dir"/ 2>/dev/null || true
            shopt -u dotglob
            rmdir "$inner" 2>/dev/null || true
        fi
    fi

    [[ -f "${release_dir}/nova-api.jar" ]] || die "解压后仍找不到 nova-api.jar"

    chown -R nova:nova "$release_dir" 2>/dev/null || true

    # 记录当前版本，回滚时要用
    local prev_release=""
    if [[ -L "$CURRENT_LINK" ]]; then
        prev_release="$(readlink -f "$CURRENT_LINK" 2>/dev/null || true)"
    fi

    step "停止服务"
    systemctl stop nova-api 2>/dev/null || true
    ok "nova-api 已停止"

    step "切换版本"
    ln -sfn "$release_dir" "$CURRENT_LINK"
    ok "current → $release_dir"

    # 静态文件：软链到 nginx 读取的路径
    local web_target="/opt/nova/app/web"
    rm -rf "$web_target" 2>/dev/null || true
    if [[ -d "${release_dir}/web" ]]; then
        ln -sfn "${release_dir}/web" "$web_target"
    else
        warn "包内没有 web 目录，前端不会更新"
    fi

    # jar 也做成软链，systemd unit 里的路径固定
    ln -sfn "${release_dir}/nova-api.jar" "${APP_DIR}/nova-api.jar"

    step "启动服务"
    if ! systemctl start nova-api; then
        err "nova-api 启动失败"
        rollback_to "$prev_release"
        die "已自动回滚到上一版本。查看日志：
  journalctl -u nova-api -n 100 --no-pager"
    fi

    step "健康检查"
    if ! wait_healthy; then
        err "健康检查失败（${HEALTH_RETRIES} 次尝试均未通过）"
        rollback_to "$prev_release"
        die "已自动回滚到上一版本。查看日志：
  journalctl -u nova-api -n 100 --no-pager"
    fi

    ok "健康检查通过"

    cleanup_old_releases

    step "部署完成"
    ok "版本 $stamp 已上线"
    info "访问：http://$(hostname -I 2>/dev/null | awk '{print $1}')/"
}

# 等待服务健康。连续成功 2 次才算通过，
# 只看一次容易被「启动过程中偶发成功」骗过去。
wait_healthy() {
    local consecutive=0
    local i

    for i in $(seq 1 "$HEALTH_RETRIES"); do
        local code
        code="$(curl -fsS -o /dev/null -w '%{http_code}' \
                --connect-timeout 3 --max-time 5 "$HEALTH_URL" 2>/dev/null || echo "000")"

        if [[ "$code" == "200" ]]; then
            consecutive=$((consecutive + 1))
            if (( consecutive >= 2 )); then
                return 0
            fi
        else
            consecutive=0
        fi

        # 服务进程挂了就不用再等了
        if ! systemctl is-active --quiet nova-api 2>/dev/null; then
            err "nova-api 进程已退出"
            return 1
        fi

        sleep "$HEALTH_INTERVAL"
    done

    return 1
}

rollback_to() {
    local target="$1"

    if [[ -z "$target" || ! -d "$target" ]]; then
        err "没有可回滚的上一版本"
        return 1
    fi

    warn "回滚到 $target"
    systemctl stop nova-api 2>/dev/null || true

    ln -sfn "$target" "$CURRENT_LINK"
    rm -rf /opt/nova/app/web
    [[ -d "${target}/web" ]] && ln -sfn "${target}/web" /opt/nova/app/web
    ln -sfn "${target}/nova-api.jar" /opt/nova/app/nova-api.jar

    systemctl start nova-api 2>/dev/null || true

    if wait_healthy; then
        ok "回滚成功，服务已恢复"
        return 0
    fi

    err "回滚后服务仍不健康，需要人工介入"
    return 1
}

cleanup_old_releases() {
    local count
    count="$(find "$RELEASES_DIR" -maxdepth 1 -mindepth 1 -type d 2>/dev/null | wc -l)"

    if (( count <= KEEP_RELEASES )); then
        return 0
    fi

    info "清理旧版本（保留最近 $KEEP_RELEASES 个）"
    find "$RELEASES_DIR" -maxdepth 1 -mindepth 1 -type d -printf '%T@ %p\n' 2>/dev/null \
        | sort -rn \
        | tail -n +$((KEEP_RELEASES + 1)) \
        | cut -d' ' -f2- \
        | while read -r old; do
            # 绝不删当前在用的
            if [[ "$(readlink -f "$CURRENT_LINK" 2>/dev/null)" != "$old" ]]; then
                rm -rf "$old"
                info "已删除 $(basename "$old")"
            fi
        done
}

# ---------------------------------------------------------------- 回滚 / 列表

do_rollback() {
    step "回滚到上一个版本"

    local current prev
    current="$(readlink -f "$CURRENT_LINK" 2>/dev/null || true)"

    prev="$(find "$RELEASES_DIR" -maxdepth 1 -mindepth 1 -type d -printf '%T@ %p\n' 2>/dev/null \
        | sort -rn | awk '{print $2}' \
        | grep -v "^${current}$" | head -1)"

    [[ -n "$prev" ]] || die "没有其他版本可回滚"

    info "当前：$(basename "$current" 2>/dev/null || echo 无)"
    info "目标：$(basename "$prev")"

    confirm "确认回滚？" n || { info "已取消"; exit 0; }

    rollback_to "$prev" || die "回滚失败"
    ok "回滚完成"
}

do_list() {
    step "历史版本"

    local current
    current="$(readlink -f "$CURRENT_LINK" 2>/dev/null || true)"

    find "$RELEASES_DIR" -maxdepth 1 -mindepth 1 -type d -printf '%T@ %p\n' 2>/dev/null \
        | sort -rn \
        | while read -r _ dir; do
            local mark="  "
            [[ "$(readlink -f "$dir")" == "$current" ]] && mark="→ "
            local size
            size="$(du -sh "$dir" 2>/dev/null | cut -f1)"
            printf '  %s%-20s %6s\n' "$mark" "$(basename "$dir")" "$size"
        done
}

# ---------------------------------------------------------------- 主流程

main() {
    require_root
    parse_args "$@"

    case "$ACTION" in
        list)
            do_list
            ;;
        rollback)
            do_rollback
            ;;
        deploy)
            [[ -n "$PACKAGE" ]] || die "请指定部署包路径，或用 --help 查看用法"
            do_deploy "$PACKAGE"
            ;;
    esac
}

main "$@"
