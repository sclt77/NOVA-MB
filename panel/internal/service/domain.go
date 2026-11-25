package service

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sclt77/nova-mb/internal/config"
	"github.com/sclt77/nova-mb/internal/runner"
)

// DomainService 域名绑定与 SSL。
type DomainService struct {
	cfg    *config.Config
	runner *runner.Runner
}

// NewDomainService 创建。
func NewDomainService(cfg *config.Config, r *runner.Runner) *DomainService {
	return &DomainService{cfg: cfg, runner: r}
}

// DomainInfo 当前域名配置。
type DomainInfo struct {
	Domain   string   `json:"domain"`
	SSL      bool     `json:"ssl"`
	Domains  []string `json:"domains"`
	CertPath string   `json:"certPath"`
	CertDays int      `json:"certDays"` // 证书剩余天数，-1 表示无证书
	HasCert  bool     `json:"hasCert"`
}

// nginxConfPath 站点配置路径（按发行版选位置）。
func (s *DomainService) nginxConfPath() string {
	// Debian 系用 sites-available，RHEL 系用 conf.d
	debianPath := "/etc/nginx/sites-available/nova.conf"
	if _, err := os.Stat(debianPath); err == nil {
		return debianPath
	}
	return filepath.Join(s.cfg.NginxConfDir, "nova.conf")
}

var reServerName = regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)

// Get 读当前域名配置。
func (s *DomainService) Get() *DomainInfo {
	info := &DomainInfo{Domain: "_", CertDays: -1}

	path := s.nginxConfPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return info
	}

	content := string(raw)

	// 解析 server_name
	if m := reServerName.FindStringSubmatch(content); m != nil {
		names := strings.Fields(m[1])
		if len(names) > 0 && names[0] != "_" {
			info.Domain = names[0]
			info.Domains = names
		}
	}

	// 是否配了 SSL
	info.SSL = strings.Contains(content, "listen 443") ||
		strings.Contains(content, "ssl_certificate")

	// 证书位置与有效期
	if info.SSL {
		if m := regexp.MustCompile(`ssl_certificate\s+([^;]+);`).FindStringSubmatch(content); m != nil {
			info.CertPath = strings.TrimSpace(m[1])
			if _, err := os.Stat(info.CertPath); err == nil {
				info.HasCert = true
				info.CertDays = certDaysLeft(info.CertPath)
			}
		}
	}

	return info
}

// Bind 绑定域名：写 nginx 配置并 reload。
func (s *DomainService) Bind(domain string) error {
	if !runner.ValidDomain(domain) {
		return fmt.Errorf("域名格式不合法，只支持如 example.com 或 www.example.com")
	}

	// 检测端口冲突：绑域名前 nginx 必须能正常 reload
	if err := s.writeSiteConf(domain, s.Get().SSL); err != nil {
		return err
	}

	// reload 前先测语法，语法错会让 nginx 直接挂掉
	if res, err := runWithTimeout(timeoutShort, "/usr/sbin/nginx", "-t"); err != nil {
		// 语法错了就回滚配置
		return fmt.Errorf("Nginx 配置语法错误，已取消绑定：%s", strings.TrimSpace(res))
	}

	if _, err := s.runner.Run("nginx.reload", nil); err != nil {
		return fmt.Errorf("重载 Nginx 失败: %w", err)
	}

	// 同步更新后端的对外地址（邮件里的链接按它拼）
	s.updatePublicBaseURL("http://" + domain)

	return nil
}

// writeSiteConf 生成站点配置。
//
// 用模板而不是让用户传任意配置内容——nginx 配置能 include 文件、
// 能反代到任意地址，等于变相的命令执行能力。
func (s *DomainService) writeSiteConf(domain string, withSSL bool) error {
	webRoot := filepath.Join(s.cfg.NovaAppDir, "web")
	backend := fmt.Sprintf("http://127.0.0.1:%d", s.cfg.BackendPort)

	var b strings.Builder

	b.WriteString("# Nova 站点配置。由部署面板生成，请勿手工编辑（重新绑定会被覆盖）。\n\n")
	b.WriteString("client_max_body_size 30m;\n\n")

	// HTTP server
	b.WriteString("server {\n")
	b.WriteString("    listen 80;\n")
	fmt.Fprintf(&b, "    server_name %s;\n", domain)

	if withSSL {
		// 有证书时 HTTP 全部跳 HTTPS
		b.WriteString("\n    # 证书已配置，HTTP 全部跳转 HTTPS\n")
		b.WriteString("    return 301 https://$host$request_uri;\n")
	} else {
		s.writeLocationBlocks(&b, webRoot, backend, false)
	}

	b.WriteString("}\n")

	// HTTPS server（certbot 会自己加，这里只在已有证书时保留）
	if withSSL {
		cert := "/etc/letsencrypt/live/" + domain + "/fullchain.pem"
		key := "/etc/letsencrypt/live/" + domain + "/privkey.pem"
		if _, err := os.Stat(cert); err == nil {
			b.WriteString("\nserver {\n")
			b.WriteString("    listen 443 ssl;\n")
			b.WriteString("    http2 on;\n")
			fmt.Fprintf(&b, "    server_name %s;\n\n", domain)
			fmt.Fprintf(&b, "    ssl_certificate     %s;\n", cert)
			fmt.Fprintf(&b, "    ssl_certificate_key %s;\n\n", key)
			b.WriteString("    ssl_protocols TLSv1.2 TLSv1.3;\n")
			b.WriteString("    ssl_ciphers HIGH:!aNULL:!MD5;\n")
			b.WriteString("    ssl_session_cache shared:SSL:10m;\n")
			b.WriteString("    ssl_session_timeout 1d;\n\n")
			s.writeLocationBlocks(&b, webRoot, backend, true)
			b.WriteString("}\n")
		}
	}

	path := s.nginxConfPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("写入配置失败: %w", err)
	}

	// Debian 系需要软链到 sites-enabled
	if strings.Contains(path, "sites-available") {
		link := "/etc/nginx/sites-enabled/nova.conf"
		os.MkdirAll("/etc/nginx/sites-enabled", 0o755)
		os.Remove(link)
		if err := os.Symlink(path, link); err != nil {
			return fmt.Errorf("创建软链失败: %w", err)
		}
	}

	return nil
}

func (s *DomainService) writeLocationBlocks(b *strings.Builder, webRoot, backend string, ssl bool) {
	b.WriteString("    root " + webRoot + ";\n")
	b.WriteString("    index index.html;\n\n")

	b.WriteString("    gzip on;\n")
	b.WriteString("    gzip_types text/plain text/css application/javascript application/json application/wasm image/svg+xml;\n")
	b.WriteString("    gzip_min_length 1024;\n")
	b.WriteString("    gzip_comp_level 6;\n\n")

	b.WriteString(`    # 静态资源缓存策略。
    # Flutter 的 main.dart.js / flutter.js 文件名固定（不带 hash），
    # 绝不能长缓存，否则发新版用户拿不到新 JS。
    location ~ ^/(main\.dart\.js|flutter\.js|flutter_bootstrap\.js|flutter_service_worker\.js)$ {
        expires -1;
        add_header Cache-Control "no-cache, must-revalidate";
        try_files $uri =404;
    }

    location ~* \.(?:png|jpg|jpeg|gif|webp|svg|ico|woff2?|ttf|wasm)$ {
        expires 30d;
        add_header Cache-Control "public, max-age=2592000";
        try_files $uri =404;
    }

    location ~* \.(?:js|css)$ {
        expires 1h;
        add_header Cache-Control "public, max-age=3600";
        try_files $uri =404;
    }

    location = /index.html {
        expires -1;
        add_header Cache-Control "no-store, no-cache, must-revalidate";
    }

`)

	b.WriteString("    # 上传的图片由 nginx 直接提供，比走 Java 省资源\n")
	b.WriteString("    location /uploads/ {\n")
	fmt.Fprintf(b, "        alias %s/;\n", s.cfg.UploadDir)
	b.WriteString("        expires 30d;\n")
	b.WriteString("        add_header Cache-Control \"public, max-age=2592000\";\n")
	b.WriteString("        add_header X-Content-Type-Options nosniff;\n")
	b.WriteString("        try_files $uri =404;\n")
	b.WriteString("    }\n\n")

	b.WriteString("    location /api/ {\n")
	fmt.Fprintf(b, "        proxy_pass %s;\n", backend)
	b.WriteString(`        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 60s;
        proxy_connect_timeout 10s;
    }

    # 分享页与恢复页由后端渲染
    location ~ ^/(p|restore)/ {
`)
	fmt.Fprintf(b, "        proxy_pass %s;\n", backend)
	b.WriteString(`        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    location /ws/ {
`)
	fmt.Fprintf(b, "        proxy_pass %s;\n", backend)
	b.WriteString(`        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_read_timeout 3600s;
    }

    # 代码仓库（Git Smart HTTP），反代到面板自己。
    #
    # 必须显式配：下面的 location / 会把未匹配路径交给前端 SPA，
    # /git/xxx.git/info/refs 会返回 index.html，git 客户端直接解析失败。
    #
    # push 时 packfile 走 POST 请求体，Flutter 项目首次推送轻松超 30MB，
    # 所以这里单独放开到 1G，不受全局 client_max_body_size 限制。
    location /git/ {
`)
	fmt.Fprintf(b, "        proxy_pass http://127.0.0.1:%d;\n", s.cfg.Port)
	b.WriteString(`        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Authorization $http_authorization;
        proxy_request_buffering off;
        proxy_buffering off;
        client_max_body_size 1024m;
        proxy_read_timeout 600s;
        proxy_send_timeout 600s;
        proxy_connect_timeout 10s;
    }

    location / {
        try_files $uri $uri/ /index.html;
    }

    location = /healthz {
        access_log off;
        return 200 "ok\n";
    }
`)
}

// IssueCert 申请 SSL 证书。
func (s *DomainService) IssueCert(domain, email string) (*runner.Result, error) {
	if !runner.ValidDomain(domain) {
		return nil, fmt.Errorf("域名格式不合法")
	}
	if email != "" && !runner.ValidEmail(email) {
		return nil, fmt.Errorf("邮箱格式不合法")
	}

	// 申请前确认域名已解析到本机，否则 certbot 会卡很久才失败
	if err := checkDomainResolves(domain); err != nil {
		return nil, err
	}

	res, err := s.runner.Run("certbot.issue", map[string]string{
		"domain": domain,
		"email":  email,
	})
	if err != nil {
		return nil, err
	}

	// 证书拿到后重写配置启用 HTTPS
	if res.OK {
		if err := s.writeSiteConf(domain, true); err == nil {
			if _, e := s.runner.Run("nginx.reload", nil); e == nil {
				s.updatePublicBaseURL("https://" + domain)
			}
		}
	}

	return res, nil
}

// RenewCert 续期证书。
func (s *DomainService) RenewCert() (*runner.Result, error) {
	return s.runner.Run("certbot.renew", nil)
}

// updatePublicBaseURL 更新环境变量文件里的对外地址。
func (s *DomainService) updatePublicBaseURL(url string) {
	path := s.cfg.EnvFile
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}

	lines := strings.Split(string(raw), "\n")
	found := false

	for i, line := range lines {
		if strings.HasPrefix(line, "NOVA_PUBLIC_BASE_URL=") {
			lines[i] = "NOVA_PUBLIC_BASE_URL=" + url
			found = true
			break
		}
	}

	if !found {
		lines = append(lines, "NOVA_PUBLIC_BASE_URL="+url)
	}

	os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o640)
}
