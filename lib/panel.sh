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
# 面板自己的数据（SQLite、会话密钥、上传的部署包），属主是面板用户
PANEL_DATA_DIR="/var/lib/nova/panel"
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
    ensure_dir "$UPLOAD_DIR" "$APP_USER:$APP_USER"     "755"
    ensure_dir "$BACKUP_DIR" "$APP_USER:$APP_USER"     "700"   # 备份含用户数据
    ensure_dir "$CONF_DIR"   "root:root"               "750"
    ensure_dir "$LOG_DIR"    "$APP_USER:$APP_USER"     "755"

    # 面板自己的数据目录必须**单独**建，并且属主是面板用户。
    #
    # 不能只建 /var/lib/nova 就指望面板自己创建子目录：
    # /var/lib/nova 属主是 nova（业务用户）、权限 755，
    # 面板以 nova-panel 身份运行，在别人的目录里建子目录会 permission denied。
    # 这个坑在真机第一次跑就踩到了，表现为面板无限重启（restart counter 一直涨）。
    ensure_dir "$DATA_DIR"        "$APP_USER:$APP_USER"     "755"
    ensure_dir "$PANEL_DATA_DIR"  "$PANEL_USER:$PANEL_USER" "700"

    # ensure_dir 对已存在的目录不会改属主，重装时会保留旧的错误权限，
    # 导致面板继续 permission denied。这里强制修正一次。
    # chown -R 是安全的：这个目录只存面板自己的数据（SQLite、会话密钥、上传包）。
    if [ -d "$PANEL_DATA_DIR" ]; then
        chown -R "$PANEL_USER:$PANEL_USER" "$PANEL_DATA_DIR" 2>/dev/null || true
        chmod 700 "$PANEL_DATA_DIR" 2>/dev/null || true
    fi

    # 部署包目录：面板上传到这里，deploy.sh 以 root 执行时读它。
    # 单独建出来并给业务用户读权限，避免把整个面板数据目录（含 SQLite、
    # 会话密钥）暴露给业务进程。
    ensure_dir "${PANEL_DATA_DIR}/packages" "$PANEL_USER:$PANEL_USER" "750"

    if command -v setfacl >/dev/null 2>&1; then
        # 只给 packages 目录递归读权限，不动 SQLite 所在的父目录
        setfacl -R -m "u:${APP_USER}:r-x" "${PANEL_DATA_DIR}/packages" 2>/dev/null || true
        setfacl -m "u:${APP_USER}:x" "$PANEL_DATA_DIR" 2>/dev/null || true
    else
        # 没有 setfacl 时退回普通权限：组读
        chmod 750 "${PANEL_DATA_DIR}/packages" 2>/dev/null || true
    fi

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

    # 本地已有二进制（离线安装 / 开发调试）就直接用
    if [[ -n "${NOVA_PANEL_BIN_LOCAL:-}" && -f "$NOVA_PANEL_BIN_LOCAL" ]]; then
        info "使用本地面板程序：$NOVA_PANEL_BIN_LOCAL"
        install -m 755 "$NOVA_PANEL_BIN_LOCAL" "$PANEL_DIR/nova-panel" \
            || die "安装面板程序失败"
        chown "$PANEL_USER:$PANEL_USER" "$PANEL_DIR/nova-panel"
        ok "面板程序已安装"
        return 0
    fi

    local bin_url="${NOVA_PANEL_BIN_URL:-}"
    local bin_sha="${NOVA_PANEL_BIN_SHA256:-}"

    # 组装下载地址列表：主地址 + 备用镜像。
    #
    # 国内服务器访问 GitHub Release 经常超时或很慢，只给一个地址
    # 等于把安装成功率赌在网络上。这里按顺序尝试，任一成功即用。
    local -a urls=()
    if [[ -n "$bin_url" ]]; then
        urls+=("$bin_url")
    else
        local base="${NOVA_RELEASE_BASE:-https://github.com/sclt77/NOVA-MB/releases/latest/download}"
        local file="nova-panel-linux-${ARCH}"
        urls+=("${base}/${file}")
        # 镜像只在没显式指定 NOVA_RELEASE_BASE 时追加，
        # 用户自己指定了源说明他知道要什么，不该被塞别的。
        if [[ -z "${NOVA_RELEASE_BASE:-}" ]]; then
            urls+=("https://ghproxy.net/${base}/${file}")
            urls+=("https://gh-proxy.com/${base}/${file}")
        fi
    fi

    local tmp_bin="/tmp/nova-panel-$$"
    info "下载面板程序（约 12MB）"

    local got=0 url
    for url in "${urls[@]}"; do
        info "  尝试：$url"
        if download "$url" "$tmp_bin" 3; then
            got=1
            bin_url="$url"
            break
        fi
        warn "  该地址失败，尝试下一个"
    done

    if [ "$got" -eq 0 ]; then
        # 必须中止，不能只警告。
        # 之前这里 return 0 继续往下装，结果是「安装完成」但面板根本不存在，
        # 用户看到成功的汇总信息却连不上，排查半天才发现是下载失败。
        err "面板程序下载失败，已尝试 ${#urls[@]} 个地址"
        err ""
        err "环境已就绪，但面板没装上。三种解决办法："
        err "  1. 检查网络后重新执行本脚本（脚本是幂等的）"
        err "  2. 手动下载后放到 $PANEL_DIR/nova-panel 并 chmod +x："
        err "     curl -L -o /tmp/np ${urls[0]}"
        err "     install -m 755 /tmp/np $PANEL_DIR/nova-panel"
        err "     chown $PANEL_USER:$PANEL_USER $PANEL_DIR/nova-panel"
        err "  3. 用国内镜像重跑："
        err "     NOVA_RELEASE_BASE=https://ghproxy.net/https://github.com/sclt77/NOVA-MB/releases/latest/download \\"
        err "       bash install.sh"
        die "安装中止：面板程序缺失"
    fi

    # 有校验值就校验，没有就提示（默认地址不提供 SHA，靠 HTTPS 保证）
    if [[ -n "$bin_sha" ]]; then
        verify_sha256 "$tmp_bin" "$bin_sha" || die "面板程序校验失败，已中止"
    else
        warn "未提供 SHA256，跳过校验（下载来源为 HTTPS）"
    fi

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
