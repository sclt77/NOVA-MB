#!/usr/bin/env bash
# 面板本体：系统用户、目录、配置、二进制、systemd unit。
#
# 关键设计：面板与 nova-api 用**不同的系统用户**。
# 面板需要写 /opt/nova、重启服务，权限比业务大；
# 业务只需要读自己的目录和写 uploads，权限越小越好。
# 万一业务被攻破，攻击者拿不到面板权限。

PANEL_USER="nova-panel"
APP_USER="nova"

PANEL_DIR="/opt/nova/panel"
APP_DIR="/opt/nova/app"
RELEASES_DIR="/opt/nova/releases"
DATA_DIR="/var/lib/nova"
UPLOAD_DIR="/var/lib/nova/uploads"
BACKUP_DIR="/var/lib/nova/backup"
CONF_DIR="/etc/nova"
LOG_DIR="/var/log/nova"

PANEL_PORT="${NOVA_PANEL_PORT:-8888}"

create_users() {
    step "创建系统用户"

    # 业务用户：不能登录 shell，只能跑服务
    if id "$APP_USER" >/dev/null 2>&1; then
        ok "用户 $APP_USER 已存在"
    else
        useradd --system --shell /usr/sbin/nologin --home-dir "$DATA_DIR" \
                --comment "Nova application" "$APP_USER" 2>/dev/null \
            || useradd --system --shell /sbin/nologin --home-dir "$DATA_DIR" \
                       --comment "Nova application" "$APP_USER" \
            || die "创建用户 $APP_USER 失败"
        ok "已创建用户 $APP_USER"
    fi

    # 面板用户：需要 sudo 执行少数几个白名单命令
    if id "$PANEL_USER" >/dev/null 2>&1; then
        ok "用户 $PANEL_USER 已存在"
    else
        useradd --system --shell /usr/sbin/nologin --home-dir "$PANEL_DIR" \
                --comment "Nova panel" "$PANEL_USER" 2>/dev/null \
            || useradd --system --shell /sbin/nologin --home-dir "$PANEL_DIR" \
                       --comment "Nova panel" "$PANEL_USER" \
            || die "创建用户 $PANEL_USER 失败"
        ok "已创建用户 $PANEL_USER"
    fi
}

create_dirs() {
    step "创建目录结构"

    ensure_dir "$PANEL_DIR"  "$PANEL_USER:$PANEL_USER" "755"
    ensure_dir "$APP_DIR"    "$APP_USER:$APP_USER"     "755"
    ensure_dir "$RELEASES_DIR" "root:root"             "755"
    ensure_dir "$DATA_DIR"   "$APP_USER:$APP_USER"     "755"
    ensure_dir "$UPLOAD_DIR" "$APP_USER:$APP_USER"     "755"
    ensure_dir "$BACKUP_DIR" "$APP_USER:$APP_USER"     "700"   # 备份含用户数据
    ensure_dir "$CONF_DIR"   "root:root"               "750"
    ensure_dir "$LOG_DIR"    "$APP_USER:$APP_USER"     "755"

    ok "目录结构就绪"
}

# sudo 白名单。
#
# 只放开面板真正需要的那几个命令，且参数固定。不要用 NOPASSWD: ALL，
# 那等于把 root 权限整个交出去，面板一被攻破就是整机沦陷。
setup_sudoers() {
    local sudoers_file="/etc/sudoers.d/nova-panel"

    ensure_file "$sudoers_file" "root:root" "440" <<EOF
# Nova 面板的最小权限白名单。
# 只允许启停指定服务、reload nginx、执行部署脚本。
# 不要改成 NOPASSWD: ALL。

${PANEL_USER} ALL=(root) NOPASSWD: /bin/systemctl start nova-api
${PANEL_USER} ALL=(root) NOPASSWD: /bin/systemctl stop nova-api
${PANEL_USER} ALL=(root) NOPASSWD: /bin/systemctl restart nova-api
${PANEL_USER} ALL=(root) NOPASSWD: /bin/systemctl status nova-api
${PANEL_USER} ALL=(root) NOPASSWD: /bin/systemctl reload nginx
${PANEL_USER} ALL=(root) NOPASSWD: /bin/systemctl restart nginx
${PANEL_USER} ALL=(root) NOPASSWD: /usr/sbin/nginx -t
${PANEL_USER} ALL=(root) NOPASSWD: /usr/sbin/nginx -s reload
${PANEL_USER} ALL=(root) NOPASSWD: /usr/bin/systemctl restart mysql
${PANEL_USER} ALL=(root) NOPASSWD: /usr/bin/systemctl restart mysqld
${PANEL_USER} ALL=(root) NOPASSWD: /opt/nova/panel/scripts/*
EOF

    # 语法检查，写错会导致 sudo 整体失效
    if command -v visudo >/dev/null 2>&1; then
        if ! visudo -c -f "$sudoers_file" >/dev/null 2>&1; then
            rm -f "$sudoers_file"
            die "sudoers 配置语法错误，已回滚"
        fi
    fi

    ok "sudo 白名单已配置"
}

# 环境变量文件。
#
# 这里存数据库密码、JWT 密钥等敏感信息，权限必须 600 且只给业务用户可读。
# 后端通过 systemd 的 EnvironmentFile 加载。
write_env_file() {
    local jwt_secret
    jwt_secret="$(gen_password 48)"

    # 已存在就不覆盖，避免重装时把正在用的密钥换掉
    # （换掉 JWT 密钥会让所有已登录用户掉线）
    if [[ -f "$CONF_DIR/env" ]]; then
        ok "环境变量文件已存在，保留现有配置"
        # 但密码变量要从已有文件读回，供后续步骤使用
        # shellcheck disable=SC1091
        set -a; . "$CONF_DIR/env" 2>/dev/null || true; set +a
        return 0
    fi

    ensure_file "$CONF_DIR/env" "root:$APP_USER" "640" <<EOF
# Nova 环境变量。由部署脚本生成。
# 权限 640：root 可写，业务用户只读。不要改成 644——里面有数据库密码。

# ---- 数据库 ----
NOVA_DB_HOST=127.0.0.1
NOVA_DB_PORT=3306
NOVA_DB_NAME=${NOVA_DB:-nova}
NOVA_DB_USER=${NOVA_DB_USER:-nova}
NOVA_DB_PASSWORD=${NOVA_DB_PASSWORD}

# ---- 安全 ----
# JWT 签名密钥。换掉会让所有用户掉线。
NOVA_JWT_SECRET=${jwt_secret}
# 生产必须为 false，否则接口异常会把堆栈吐给调用方
NOVA_DEV_MODE=false
# 在 Nginx 后面，要信任 X-Forwarded-For 才能拿到真实客户端 IP
NOVA_TRUST_PROXY=true
# 对外域名，等域名绑定后由面板更新
NOVA_PUBLIC_BASE_URL=http://localhost

# ---- 路径 ----
NOVA_UPLOAD_DIR=${UPLOAD_DIR}
NOVA_TRASH_RETENTION_DAYS=15

# ---- 面板 ----
NOVA_PANEL_PORT=${PANEL_PORT}
EOF

    # 把密码变量回传，让后续步骤能用
    export NOVA_DB_PASSWORD
    ok "环境变量文件已写入（权限 640）"
}

install_panel_binary() {
    step "安装面板程序"

    local bin_url="${NOVA_PANEL_BIN_URL:-}"
    local bin_sha="${NOVA_PANEL_BIN_SHA256:-}"

    if [[ -z "$bin_url" ]]; then
        warn "未指定面板二进制下载地址（NOVA_PANEL_BIN_URL），跳过面板程序安装"
        warn "环境已就绪，可稍后手动部署面板。"
        return 0
    fi

    local tmp_bin="/tmp/nova-panel-$$"
    info "下载面板程序"
    download "$bin_url" "$tmp_bin" || die "面板程序下载失败：$bin_url"

    verify_sha256 "$tmp_bin" "$bin_sha" || die "面板程序校验失败，已中止"

    install -m 755 "$tmp_bin" "$PANEL_DIR/nova-panel" || die "安装面板程序失败"
    rm -f "$tmp_bin"
    chown "$PANEL_USER:$PANEL_USER" "$PANEL_DIR/nova-panel"

    ok "面板程序已安装：$PANEL_DIR/nova-panel"
}

# 运维脚本：面板调它们执行实际操作。
install_scripts() {
    step "安装运维脚本"

    local script_src="${NOVA_SCRIPTS_DIR:-}"
    ensure_dir "$PANEL_DIR/scripts" "$PANEL_USER:$PANEL_USER" "755"

    if [[ -n "$script_src" && -d "$script_src" ]]; then
        cp -f "$script_src"/*.sh "$PANEL_DIR/scripts/" 2>/dev/null || true
        chmod 755 "$PANEL_DIR/scripts"/*.sh 2>/dev/null || true
        chown -R "$PANEL_USER:$PANEL_USER" "$PANEL_DIR/scripts"
        ok "运维脚本已安装：$(ls -1 "$PANEL_DIR/scripts" | wc -l) 个"
    else
        warn "未提供运维脚本目录，跳过"
    fi
}

write_systemd_units() {
    step "配置 systemd 服务"

    # ---- 面板服务 ----
    local panel_unit="/etc/systemd/system/nova-panel.service"
    if [[ -x "$PANEL_DIR/nova-panel" ]]; then
        ensure_file "$panel_unit" "root:root" "644" <<EOF
[Unit]
Description=Nova Deployment Panel
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${PANEL_USER}
Group=${PANEL_USER}
WorkingDirectory=${PANEL_DIR}
EnvironmentFile=-${CONF_DIR}/panel.env
ExecStart=${PANEL_DIR}/nova-panel
Restart=always
RestartSec=5
# 面板崩了要能自动起来，它是你修复业务的唯一入口

# 安全加固：即使面板被攻破，攻击面也受限
NoNewPrivileges=false
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ReadWritePaths=${PANEL_DIR} ${RELEASES_DIR} ${DATA_DIR} ${CONF_DIR} ${LOG_DIR} /etc/nginx /etc/systemd/system
# 注意：NoNewPrivileges 必须为 false，否则 sudo 白名单失效

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        systemctl enable nova-panel >/dev/null 2>&1 || true
        ok "面板服务已注册"
    else
        warn "面板程序未安装，跳过面板服务注册"
    fi

    # ---- 业务服务 ----
    local app_unit="/etc/systemd/system/nova-api.service"
    ensure_file "$app_unit" "root:root" "644" <<EOF
[Unit]
Description=Nova API Server
After=network-online.target mysql.service mysqld.service
Wants=network-online.target

[Service]
Type=simple
User=${APP_USER}
Group=${APP_USER}
WorkingDirectory=${APP_DIR}
EnvironmentFile=${CONF_DIR}/env

# 堆大小按机器内存自适应：2G 给 512M，4G 给 768M
# SerialGC 在小堆上比 G1 省几十 MB 内存，且没有大堆延迟问题
ExecStart=${JAVA_HOME}/bin/java \\
    -Xms256m -Xmx${APP_HEAP:-512m} \\
    -XX:MaxMetaspaceSize=192m \\
    -XX:+UseSerialGC \\
    -jar ${APP_DIR}/nova-api.jar

Restart=always
RestartSec=10
# 启动慢时不要过早判定失败（Flyway 迁移可能耗时）
TimeoutStartSec=180

# 业务只需要读自己的目录、写 uploads 和日志
ReadWritePaths=${DATA_DIR} ${LOG_DIR}
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable nova-api >/dev/null 2>&1 || true
    ok "业务服务已注册"
}

setup_logrotate() {
    step "配置日志轮转"

    ensure_file /etc/logrotate.d/nova "root:root" "644" <<EOF
# 不轮转的话日志会一直涨，最后把磁盘写满。
${LOG_DIR}/*.log {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
EOF

    ok "日志轮转已配置（保留 14 天）"
}
