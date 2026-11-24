#!/usr/bin/env bash
# MySQL 8。
#
# 两个要点：
# 1. **2G 内存机器必须调低 innodb_buffer_pool_size**。MySQL 8 默认 128M，
#    但配合其他参数和连接缓冲，小机器上容易和后端 JVM 抢内存导致 OOM。
# 2. **只监听 127.0.0.1**。数据库绝不能对公网开放，面板和业务都在本机。

MYSQL_ROOT_CNF="/etc/nova/.mysql-root.cnf"
NOVA_DB="nova"
NOVA_DB_USER="nova"

install_mysql() {
    step "安装 MySQL 8"

    if command -v mysql >/dev/null 2>&1 && systemctl is-active --quiet mysql 2>/dev/null; then
        ok "MySQL 已在运行，跳过安装"
    elif command -v mysql >/dev/null 2>&1 && systemctl is-active --quiet mysqld 2>/dev/null; then
        ok "MySQL 已在运行，跳过安装"
    else
        install_mysql_packages
    fi

    configure_mysql
    ensure_mysql_running
    init_database
}

install_mysql_packages() {
    case "$OS_FAMILY" in
        debian)
            # Ubuntu/Debian 官方源里的 mysql-server 就是 8.0
            if ! pkg_install mysql-server; then
                die "MySQL 安装失败。请检查软件源。"
            fi
            ;;
        rhel)
            # RHEL 系默认源里是 MariaDB，必须装官方 MySQL 8 源
            info "添加 MySQL 官方软件源"
            local repo_rpm="https://repo.mysql.com/mysql80-community-release-el${OS_VERSION%%.*}-1.noarch.rpm"
            if ! download "$repo_rpm" /tmp/mysql-repo.rpm; then
                die "下载 MySQL 软件源失败。请检查网络。"
            fi
            if ! rpm -Uvh --replacepkgs /tmp/mysql-repo.rpm >/dev/null 2>&1; then
                die "安装 MySQL 软件源失败"
            fi
            rm -f /tmp/mysql-repo.rpm

            # MySQL 8.0 源默认禁用，需要显式启用
            if command -v dnf >/dev/null 2>&1; then
                dnf config-manager --enable mysql80-community >/dev/null 2>&1 || true
            fi

            if ! pkg_install mysql-community-server; then
                die "MySQL 安装失败。如果提示 GPG 校验失败，请执行：
  rpm --import https://repo.mysql.com/RPM-GPG-KEY-mysql-2023"
            fi
            ;;
    esac

    ok "MySQL 软件包安装完成"
}

configure_mysql() {
    local cnf_dir="/etc/mysql/mysql.conf.d"
    [[ "$OS_FAMILY" == "rhel" ]] && cnf_dir="/etc/my.cnf.d"
    ensure_dir "$cnf_dir"

    # 小内存机器调低缓冲池：2G 给 128M，4G 给 256M
    local buffer_pool="256M"
    (( LOW_MEMORY == 1 )) && buffer_pool="128M"

    ensure_file "$cnf_dir/nova.cnf" "root:root" "644" <<EOF
# Nova 部署脚本生成。改完需要重启 mysql 生效。
[mysqld]
# 只监听本机。数据库绝不能对公网开放。
bind-address = 127.0.0.1

# 缓冲池：小机器上这是最大的一块内存占用
innodb_buffer_pool_size = ${buffer_pool}

# 单条 SQL 包上限，要和后端的 multipart 上限匹配
max_allowed_packet = 64M

# 时区：后端 JDBC 用 Asia/Shanghai，服务端保持一致避免时间错位
default-time-zone = '+08:00'

# 慢查询：超过 2 秒记下来，排障用
slow_query_log = 1
slow_query_log_file = /var/log/mysql/nova-slow.log
long_query_time = 2

# 字符集：utf8mb4 才能存 emoji
character-set-server = utf8mb4
collation-server = utf8mb4_unicode_ci

[client]
default-character-set = utf8mb4
EOF

    ok "MySQL 配置已写入 $cnf_dir/nova.cnf（缓冲池 ${buffer_pool}）"
}

ensure_mysql_running() {
    local svc="mysql"
    systemctl list-unit-files 2>/dev/null | grep -q '^mysqld\.service' && svc="mysqld"

    systemctl enable "$svc" >/dev/null 2>&1 || true
    systemctl restart "$svc" || die "MySQL 启动失败。查看日志：
  journalctl -u $svc -n 50 --no-pager
  tail -50 /var/log/mysql/error.log"

    # 等它真的能接受连接，最多 60 秒
    local i
    for i in $(seq 1 60); do
        if mysqladmin ping >/dev/null 2>&1; then
            ok "MySQL 已启动"
            return 0
        fi
        sleep 1
    done

    die "MySQL 启动后 60 秒内无法连接。查看日志：
  journalctl -u $svc -n 50 --no-pager"
}

# 拿到能执行 SQL 的 root 连接方式。
# 新装的 MySQL 8 在 Debian 系用 auth_socket，RHEL 系用临时密码。
mysql_root_exec() {
    mysql --defaults-file="$MYSQL_ROOT_CNF" -N -B -e "$1" 2>/dev/null
}

setup_root_access() {
    ensure_dir "/etc/nova" "root:root" "750"

    # 已经有可用的 root 配置就直接用
    if [[ -f "$MYSQL_ROOT_CNF" ]] && mysql_root_exec "SELECT 1" >/dev/null 2>&1; then
        return 0
    fi

    # 尝试 1：socket 认证（Debian 系默认，root 直接 mysql 就能进）
    if mysql -N -B -e "SELECT 1" >/dev/null 2>&1; then
        NOVA_DB_PASSWORD="${NOVA_DB_PASSWORD:-$(gen_password 20)}"
        mysql -e "
            ALTER USER 'root'@'localhost' IDENTIFIED WITH mysql_native_password BY '${NOVA_DB_PASSWORD}';
        " 2>/dev/null || true
        cat >"$MYSQL_ROOT_CNF" <<EOF
[client]
user=root
password=${NOVA_DB_PASSWORD}
EOF
        chmod 600 "$MYSQL_ROOT_CNF"
        if mysql_root_exec "SELECT 1" >/dev/null 2>&1; then
            return 0
        fi
    fi

    # 尝试 2：RHEL 系从日志里取临时密码
    local tmp_pass=""
    if [[ -f /var/log/mysqld.log ]]; then
        tmp_pass="$(grep 'temporary password' /var/log/mysqld.log 2>/dev/null | tail -1 | awk '{print $NF}')"
    fi

    if [[ -n "$tmp_pass" ]]; then
        NOVA_DB_PASSWORD="${NOVA_DB_PASSWORD:-$(gen_password 20)}"
        if mysql --connect-expired-password -uroot -p"$tmp_pass" -e "
            ALTER USER 'root'@'localhost' IDENTIFIED BY '${NOVA_DB_PASSWORD}';
        " 2>/dev/null; then
            cat >"$MYSQL_ROOT_CNF" <<EOF
[client]
user=root
password=${NOVA_DB_PASSWORD}
EOF
            chmod 600 "$MYSQL_ROOT_CNF"
            if mysql_root_exec "SELECT 1" >/dev/null 2>&1; then
                return 0
            fi
        fi
    fi

    die "无法获得 MySQL root 权限。请手动设置 root 密码后重试，或创建
$MYSQL_ROOT_CNF 写入 [client] user=root password=你的密码"
}

init_database() {
    setup_root_access

    NOVA_DB_PASSWORD="${NOVA_DB_PASSWORD:-$(gen_password 24)}"

    # 建库建用户。全部用 IF NOT EXISTS，重复执行不会报错也不会改密码。
    # 但如果库已存在，说明是重装/升级，此时**不改密码**，否则会把
    # 正在运行的后端连不上库。
    local db_exists
    db_exists="$(mysql_root_exec "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='${NOVA_DB}'")"

    if [[ "$db_exists" == "1" ]]; then
        ok "数据库 ${NOVA_DB} 已存在，保留现有数据"

        # 从已有配置里读回密码，别覆盖
        if [[ -f /etc/nova/env ]] && grep -q '^NOVA_DB_PASSWORD=' /etc/nova/env; then
            NOVA_DB_PASSWORD="$(grep '^NOVA_DB_PASSWORD=' /etc/nova/env | head -1 | cut -d= -f2-)"
            ok "沿用已有数据库密码"
        else
            # 库在但没记录密码，重置成新密码
            warn "数据库已存在但找不到密码记录，将重置 nova 用户密码"
            mysql_root_exec "
                ALTER USER '${NOVA_DB_USER}'@'localhost' IDENTIFIED BY '${NOVA_DB_PASSWORD}';
            " || true
        fi
    else
        info "创建数据库与用户"
        mysql_root_exec "
            CREATE DATABASE IF NOT EXISTS \`${NOVA_DB}\`
                CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
            CREATE USER IF NOT EXISTS '${NOVA_DB_USER}'@'localhost'
                IDENTIFIED BY '${NOVA_DB_PASSWORD}';
            GRANT ALL PRIVILEGES ON \`${NOVA_DB}\`.* TO '${NOVA_DB_USER}'@'localhost';
            FLUSH PRIVILEGES;
        " || die "创建数据库失败"

        ok "数据库 ${NOVA_DB} 与用户 ${NOVA_DB_USER} 创建完成"
    fi
}
