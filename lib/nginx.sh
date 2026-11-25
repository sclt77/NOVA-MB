#!/usr/bin/env bash
# Nginx。
#
# 只做两件事：托管前端静态文件、反代 /api 到后端。
# 不在这里配 SSL——证书要等域名解析生效后才能申请，属于部署后步骤。

install_nginx() {
    step "安装 Nginx"

    if command -v nginx >/dev/null 2>&1; then
        ok "Nginx 已安装，跳过"
    else
        pkg_install nginx || die "Nginx 安装失败"
        ok "Nginx 安装完成"
    fi

    systemctl enable nginx >/dev/null 2>&1 || true

    # 默认站点会占用 80 端口，先禁掉
    if [[ -L /etc/nginx/sites-enabled/default ]]; then
        rm -f /etc/nginx/sites-enabled/default
        info "已禁用 Nginx 默认站点"
    fi

    # RHEL 系的 nginx.conf 里 server 块配置在 conf.d，确保目录存在
    ensure_dir /etc/nginx/conf.d
}

# 写 Nova 的站点配置。
#
# 参数：$1 域名（可以是 _ 表示任意），$2 后端端口
write_nginx_site() {
    local domain="${1:-_}"
    local backend_port="${2:-8080}"
    local web_root="/opt/nova/app/web"

    # 面板端口：/git/ 要反代到这里。
    # 用 NOVA_PANEL_PORT 环境变量（安装脚本里已设置），默认 8888。
    local panel_port="${NOVA_PANEL_PORT:-8888}"

    local conf_path="/etc/nginx/conf.d/nova.conf"
    [[ "$OS_FAMILY" == "debian" ]] && conf_path="/etc/nginx/sites-available/nova.conf"

    ensure_file "$conf_path" "root:root" "644" <<EOF
# Nova 站点配置。由部署面板生成，改完执行 nginx -t && systemctl reload nginx。

# 上传大小要和后端 spring.servlet.multipart.max-request-size 对齐（30MB）
client_max_body_size 30m;

server {
    listen 80;
    server_name ${domain};

    # 前端静态文件
    root ${web_root};
    index index.html;

    # gzip：Flutter Web 的 main.dart.js 有 4MB，压缩后约 1MB
    gzip on;
    gzip_types text/plain text/css application/javascript application/json
               application/wasm image/svg+xml;
    gzip_min_length 1024;
    gzip_comp_level 6;

    # 静态资源缓存策略。
    #
    # 关键点：**不能给 main.dart.js / flutter.js 设长缓存**。
    # Flutter Web 产物的文件名是固定的（不带内容 hash），
    # 设 7 天缓存后发新版，用户浏览器会一直用旧 JS，
    # 表现为「代码改了但界面没变」——首次部署就踩过这个坑。
    #
    # 只有文件名带 hash 的资源（如图片、字体）才适合长缓存。

    # Flutter 的核心 JS：必须每次校验，不能长缓存
    location ~ ^/(main\.dart\.js|flutter\.js|flutter_bootstrap\.js|flutter_service_worker\.js)$ {
        expires -1;
        add_header Cache-Control "no-cache, must-revalidate";
        try_files \$uri =404;
    }

    # 图片、字体、wasm：内容变了文件名通常也变，可以长缓存
    location ~* \.(?:png|jpg|jpeg|gif|webp|svg|ico|woff2?|ttf|wasm)\$ {
        expires 30d;
        add_header Cache-Control "public, max-age=2592000";
        try_files \$uri =404;
    }

    # 其余 JS/CSS（通常是框架或插件）：中等缓存
    location ~* \.(?:js|css)\$ {
        expires 1h;
        add_header Cache-Control "public, max-age=3600";
        try_files \$uri =404;
    }

    location = /index.html {
        expires -1;
        add_header Cache-Control "no-store, no-cache, must-revalidate";
    }

    # 上传的图片：由后端目录直接提供，比走 Java 省资源
    location /uploads/ {
        alias /var/lib/nova/uploads/;
        expires 30d;
        add_header Cache-Control "public, max-age=2592000";
        # 图片不需要执行任何东西
        add_header X-Content-Type-Options nosniff;
        try_files \$uri =404;
    }

    # 后端 API
    location /api/ {
        proxy_pass http://127.0.0.1:${backend_port};
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_read_timeout 60s;
        proxy_connect_timeout 10s;
    }

    # 分享页与恢复页是后端渲染的 HTML
    location ~ ^/(p|restore)/ {
        proxy_pass http://127.0.0.1:${backend_port};
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
    }

    # WebSocket
    location /ws/ {
        proxy_pass http://127.0.0.1:${backend_port};
        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host \$host;
        proxy_read_timeout 3600s;
    }

    # 代码仓库（Git Smart HTTP）
    #
    # 反代到面板的 8888 端口，不是后端 8080。
    # 仓库功能由面板自己实现（git http-backend），不是业务后端的一部分。
    #
    # 必须显式配这一段：下面 location / 会把所有未匹配路径交给前端 SPA，
    # 导致 /git/xxx.git/info/refs 返回 index.html，git 客户端报
    # "not a git repository" 或直接解析失败。
    #
    # client_max_body_size 要给足：push 时整个 packfile 走 POST 请求体，
    # Flutter 项目首次推送轻松超过 30MB，用全局的 30m 会被 413 拦掉。
    location /git/ {
        proxy_pass http://127.0.0.1:${panel_port};
        proxy_http_version 1.1;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;

        # Git 协议需要透传这些头，否则 push 会失败
        proxy_set_header Authorization \$http_authorization;
        proxy_request_buffering off;
        client_max_body_size 1024m;

        # clone 大仓库可能持续几分钟
        proxy_read_timeout 600s;
        proxy_send_timeout 600s;
        proxy_connect_timeout 10s;

        # 禁用缓冲：packfile 是流式的，缓冲会让内存暴涨且延迟变大
        proxy_buffering off;
    }

    # 前端单页应用：找不到文件时回落到 index.html
    location / {
        try_files \$uri \$uri/ /index.html;
    }

    # 健康检查给负载均衡用，不记日志避免刷屏
    location = /healthz {
        access_log off;
        return 200 "ok\n";
    }
}
EOF

    # Debian 系需要软链到 sites-enabled
    if [[ "$OS_FAMILY" == "debian" ]]; then
        ensure_dir /etc/nginx/sites-enabled
        ln -sf /etc/nginx/sites-available/nova.conf /etc/nginx/sites-enabled/nova.conf
    fi

    # 配置有语法错误时不要 reload，否则 nginx 会直接挂掉
    if ! nginx -t >/dev/null 2>&1; then
        err "Nginx 配置语法错误："
        nginx -t 2>&1 | sed 's/^/    /' >&2
        die "请检查 $conf_path"
    fi

    systemctl reload nginx 2>/dev/null || systemctl restart nginx || die "Nginx 启动失败"
    ok "Nginx 站点已配置（域名：${domain}，后端端口：${backend_port}）"
}
