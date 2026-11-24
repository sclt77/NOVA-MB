package runner

import (
	"fmt"
	"time"

	"github.com/sclt77/nova-mb/internal/config"
)

// RegisterDefaults 注册面板所有允许的动作。
//
// 这里是**唯一**决定面板能做什么的地方。想加新能力就在这加一个 Action，
// 想收紧权限就删掉对应的 Action。审查安全性只需要看这一个函数。
func RegisterDefaults(r *Runner, cfg *config.Config) {
	registerServices(r, cfg)
	registerScripts(r, cfg)
	registerCerts(r)
}

// registerServices 服务启停。
func registerServices(r *Runner, cfg *config.Config) {
	// 允许操作的服务白名单。
	// 不做成任意服务名参数，避免被用来启停系统关键服务。
	services := map[string]bool{
		cfg.APIService:   true,
		cfg.NginxService: true,
		cfg.MySQLService: true,
		"mysqld":         true, // RHEL 系叫 mysqld
	}

	serviceAction := func(verb string) func(map[string]string) ([]string, error) {
		return func(in map[string]string) ([]string, error) {
			svc := in["service"]
			if !services[svc] {
				return nil, fmt.Errorf("不允许操作该服务")
			}
			return []string{verb, svc}, nil
		}
	}

	r.Register(Action{
		Name:     "service.start",
		Bin:      "/bin/systemctl",
		ArgsFunc: serviceAction("start"),
		Timeout:  90 * time.Second,
		Display:  "systemctl start <服务>",
	})
	r.Register(Action{
		Name:     "service.stop",
		Bin:      "/bin/systemctl",
		ArgsFunc: serviceAction("stop"),
		Timeout:  90 * time.Second,
		Display:  "systemctl stop <服务>",
	})
	r.Register(Action{
		Name:     "service.restart",
		Bin:      "/bin/systemctl",
		ArgsFunc: serviceAction("restart"),
		Timeout:  180 * time.Second,
		Display:  "systemctl restart <服务>",
	})

	// nginx 配置重载：改完站点配置必须 reload，不能 restart
	r.Register(Action{
		Name:    "nginx.reload",
		Bin:     "/bin/systemctl",
		Args:    []string{"reload", "nginx"},
		Timeout: 30 * time.Second,
		Display: "systemctl reload nginx",
	})
}

// registerScripts 运维脚本。
//
// 脚本参数由服务层校验后传入，这里再做一次长度与字符检查（第二道防线）。
func registerScripts(r *Runner, cfg *config.Config) {
	script := func(name string) string { return cfg.ScriptsDir + "/" + name }

	// ---- 部署 ----

	r.Register(Action{
		Name: "deploy",
		Bin:  script("deploy.sh"),
		ArgsFunc: func(in map[string]string) ([]string, error) {
			pkg := in["package"]
			if pkg == "" || len(pkg) > 512 || ContainsShellMeta(pkg) {
				return nil, fmt.Errorf("包路径不合法")
			}
			return []string{pkg}, nil
		},
		Timeout: 10 * time.Minute,
		Display: "deploy.sh <部署包>",
	})

	r.Register(Action{
		Name:    "rollback",
		Bin:     script("deploy.sh"),
		Args:    []string{"--rollback"},
		Timeout: 10 * time.Minute,
		Display: "deploy.sh --rollback",
	})

	// ---- 备份 ----

	r.Register(Action{
		Name:    "backup.full",
		Bin:     script("backup.sh"),
		Timeout: 30 * time.Minute,
		Display: "backup.sh",
	})
	r.Register(Action{
		Name:    "backup.quick",
		Bin:     script("backup.sh"),
		Args:    []string{"--quick"},
		Timeout: 15 * time.Minute,
		Display: "backup.sh --quick",
	})
	r.Register(Action{
		Name:    "backup.verify",
		Bin:     script("backup.sh"),
		Args:    []string{"--verify"},
		Timeout: 30 * time.Minute,
		Display: "backup.sh --verify",
	})
	r.Register(Action{
		Name:    "backup.list",
		Bin:     script("backup.sh"),
		Args:    []string{"--list"},
		Timeout: 30 * time.Second,
		Display: "backup.sh --list",
	})

	// ---- 恢复 ----

	r.Register(Action{
		Name: "restore",
		Bin:  script("restore.sh"),
		ArgsFunc: func(in map[string]string) ([]string, error) {
			stamp := in["stamp"]
			if !ValidStamp(stamp) {
				return nil, fmt.Errorf("备份时间戳格式不合法")
			}
			scope := in["scope"]
			switch scope {
			case "db", "uploads", "all":
			default:
				return nil, fmt.Errorf("恢复范围不合法")
			}
			return []string{"--" + scope, stamp}, nil
		},
		Timeout: 30 * time.Minute,
		Display: "restore.sh --<范围> <时间戳>",
	})

	// ---- 健康检查 ----

	r.Register(Action{
		Name:    "healthcheck",
		Bin:     script("healthcheck.sh"),
		Args:    []string{"--json"},
		Timeout: 30 * time.Second,
		Display: "healthcheck.sh --json",
	})

	// ---- 定时任务 ----

	r.Register(Action{
		Name:    "cron.install",
		Bin:     script("cron.sh"),
		Args:    []string{"install"},
		Timeout: 30 * time.Second,
		Display: "cron.sh install",
	})
	r.Register(Action{
		Name:    "cron.remove",
		Bin:     script("cron.sh"),
		Args:    []string{"remove"},
		Timeout: 30 * time.Second,
		Display: "cron.sh remove",
	})
	r.Register(Action{
		Name:    "cron.list",
		Bin:     script("cron.sh"),
		Args:    []string{"list"},
		Timeout: 30 * time.Second,
		Display: "cron.sh list",
	})
}

// registerCerts SSL 证书。
func registerCerts(r *Runner) {
	r.Register(Action{
		Name: "certbot.issue",
		Bin:  "/usr/bin/certbot",
		ArgsFunc: func(in map[string]string) ([]string, error) {
			domain := in["domain"]
			if !ValidDomain(domain) {
				return nil, fmt.Errorf("域名格式不合法")
			}
			email := in["email"]
			if email != "" && !ValidEmail(email) {
				return nil, fmt.Errorf("邮箱格式不合法")
			}

			args := []string{
				"--nginx",
				"-d", domain,
				"--non-interactive",
				"--agree-tos",
				"--redirect",
			}
			if email != "" {
				args = append(args, "--email", email)
			} else {
				// 不填邮箱用注册模式，避免卡在交互提示
				args = append(args, "--register-unsafely-without-email")
			}
			return args, nil
		},
		Timeout: 5 * time.Minute,
		Display: "certbot --nginx -d <域名>",
	})

	r.Register(Action{
		Name:    "certbot.renew",
		Bin:     "/usr/bin/certbot",
		Args:    []string{"renew", "--quiet"},
		Timeout: 5 * time.Minute,
		Display: "certbot renew",
	})
}
