// Package config 加载与校验面板配置。
//
// 配置来源优先级：环境变量 > 配置文件 > 默认值。
// 面板的配置很少，不值得引入配置库，手写解析反而更可控。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config 面板运行配置。
type Config struct {
	// 监听地址。默认只绑 127.0.0.1，要对外必须显式改成 0.0.0.0
	Listen string

	// 面板端口
	Port int

	// 数据目录：存面板自己的数据库（用户、审计日志）
	DataDir string

	// 是否启用 HTTPS。面板存 Cookie，生产必须开
	UseTLS   bool
	CertFile string
	KeyFile  string

	// 会话签名密钥，用于 Cookie 加密。首次启动自动生成。
	SessionKey string

	// 面板部署路径
	NovaAppDir   string
	ScriptsDir   string
	EnvFile      string
	UploadDir    string
	BackupDir    string
	LogDir       string
	NginxConfDir string
	ReleasesDir  string

	// 服务名
	APIService   string
	NginxService string
	MySQLService string

	// 后端端口（反代目标）
	BackendPort int

	// 部署包上传大小上限（字节）
	MaxUploadBytes int64
}

// Load 读取配置。cfgPath 为空时只用环境变量与默认值。
func Load(cfgPath string) (*Config, error) {
	c := defaults()

	if cfgPath != "" {
		if err := loadFile(c, cfgPath); err != nil {
			return nil, err
		}
	}

	applyEnv(c)

	if err := validate(c); err != nil {
		return nil, err
	}

	return c, nil
}

func defaults() *Config {
	return &Config{
		Listen:         "127.0.0.1",
		Port:           8888,
		DataDir:        "/var/lib/nova/panel",
		UseTLS:         false,
		CertFile:       "",
		KeyFile:        "",
		NovaAppDir:     "/opt/nova/app",
		ScriptsDir:     "/opt/nova/panel/scripts",
		EnvFile:        "/etc/nova/env",
		UploadDir:      "/var/lib/nova/uploads",
		BackupDir:      "/var/lib/nova/backup",
		LogDir:         "/var/log/nova",
		NginxConfDir:   "/etc/nginx/conf.d",
		ReleasesDir:    "/opt/nova/releases",
		APIService:     "nova-api",
		NginxService:   "nginx",
		MySQLService:   "mysql",
		BackendPort:    8080,
		MaxUploadBytes: 500 << 20, // 500MB
	}
}

// loadFile 解析简单的 key=value 配置。
//
// 不引入 TOML/YAML 库：配置项就这么十几个，手写解析省掉一个依赖，
// 也避免用户被复杂的嵌套语法绊住。
func loadFile(c *Config, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 没有配置文件不算错
		}
		return fmt.Errorf("读取配置 %s: %w", path, err)
	}

	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d 格式错误，应为 key=value", path, lineNo+1)
		}

		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)

		if err := set(c, key, value); err != nil {
			return fmt.Errorf("%s:%d %w", path, lineNo+1, err)
		}
	}

	return nil
}

func set(c *Config, key, value string) error {
	switch key {
	case "listen":
		c.Listen = value
	case "port":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("port 必须是数字：%q", value)
		}
		c.Port = n
	case "data_dir":
		c.DataDir = value
	case "use_tls":
		c.UseTLS = value == "true" || value == "1"
	case "cert_file":
		c.CertFile = value
	case "key_file":
		c.KeyFile = value
	case "session_key":
		c.SessionKey = value
	case "nova_app_dir":
		c.NovaAppDir = value
	case "scripts_dir":
		c.ScriptsDir = value
	case "env_file":
		c.EnvFile = value
	case "upload_dir":
		c.UploadDir = value
	case "backup_dir":
		c.BackupDir = value
	case "log_dir":
		c.LogDir = value
	case "nginx_conf_dir":
		c.NginxConfDir = value
	case "releases_dir":
		c.ReleasesDir = value
	case "api_service":
		c.APIService = value
	case "nginx_service":
		c.NginxService = value
	case "mysql_service":
		c.MySQLService = value
	case "backend_port":
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("backend_port 必须是数字：%q", value)
		}
		c.BackendPort = n
	default:
		// 未知配置项直接报错，避免拼错了却静默生效
		return fmt.Errorf("未知配置项：%s", key)
	}
	return nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("NOVA_PANEL_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("NOVA_PANEL_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Port = n
		}
	}
	if v := os.Getenv("NOVA_PANEL_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("NOVA_PANEL_TLS"); v == "true" || v == "1" {
		c.UseTLS = true
	}
	if v := os.Getenv("NOVA_PANEL_CERT"); v != "" {
		c.CertFile = v
	}
	if v := os.Getenv("NOVA_PANEL_KEY"); v != "" {
		c.KeyFile = v
	}
	if v := os.Getenv("NOVA_PANEL_SESSION_KEY"); v != "" {
		c.SessionKey = v
	}
}

func validate(c *Config) error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("端口不合法：%d", c.Port)
	}
	if c.UseTLS && (c.CertFile == "" || c.KeyFile == "") {
		return fmt.Errorf("启用 HTTPS 时必须同时配置证书与私钥")
	}

	// 数据目录必须能写，否则用户登录都存不下来
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("无法创建数据目录 %s: %w", c.DataDir, err)
	}

	// 会话密钥：没有就用配置里的，再没有就生成并落盘，
	// 保证重启后已登录的会话不失效。
	if c.SessionKey == "" {
		key, err := loadOrCreateSessionKey(c.DataDir)
		if err != nil {
			return err
		}
		c.SessionKey = key
	}

	return nil
}

func loadOrCreateSessionKey(dir string) (string, error) {
	path := filepath.Join(dir, "session.key")

	if raw, err := os.ReadFile(path); err == nil {
		s := strings.TrimSpace(string(raw))
		if len(s) >= 32 {
			return s, nil
		}
	}

	key, err := randomHex(32)
	if err != nil {
		return "", err
	}

	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return "", fmt.Errorf("写入会话密钥失败: %w", err)
	}

	return key, nil
}

// Addr 返回监听地址。
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Listen, c.Port)
}
