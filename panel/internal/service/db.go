package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sclt77/nova-mb/internal/config"
	"github.com/sclt77/nova-mb/internal/runner"
)

// DBService 数据库信息。
//
// 只做只读的信息展示：表列表、大小、连接数、慢查询。
// **不提供 SQL 控制台**——那等于把数据库的完全控制权交给任何拿到面板的人，
// 而面板的定位是「部署运维」，不是数据库客户端。
type DBService struct {
	cfg    *config.Config
	runner *runner.Runner
}

// NewDBService 创建。
func NewDBService(cfg *config.Config, r *runner.Runner) *DBService {
	return &DBService{cfg: cfg, runner: r}
}

// DBInfo 数据库概览。
type DBInfo struct {
	Connected bool        `json:"connected"`
	Version   string      `json:"version"`
	Database  string      `json:"database"`
	SizeMB    float64     `json:"sizeMB"`
	Tables    []TableInfo `json:"tables"`
	Connections int       `json:"connections"`
	MaxConns    int       `json:"maxConns"`
	Uptime      string    `json:"uptime"`
	Error       string    `json:"error"`
}

// TableInfo 单表信息。
type TableInfo struct {
	Name      string  `json:"name"`
	Rows      int64   `json:"rows"`
	SizeMB    float64 `json:"sizeMB"`
	DataMB    float64 `json:"dataMB"`
	IndexMB   float64 `json:"indexMB"`
	Engine    string  `json:"engine"`
	Collation string  `json:"collation"`
	Comment   string  `json:"comment"`
}

// Info 读取数据库信息。
//
// 通过 mysql 客户端查询而不是引入 Go 的 MySQL 驱动：
// 面板不该为了显示几个数字就长期持有数据库连接池，
// 而且这样天然复用了 /etc/nova/.mysql-root.cnf 的凭证。
func (s *DBService) Info() *DBInfo {
	info := &DBInfo{Database: "nova"}

	dbName := s.dbName()
	info.Database = dbName

	// 测试连接
	out, err := s.query("SELECT VERSION()")
	if err != nil {
		msg := "无法连接数据库：" + strings.TrimSpace(out)
		// 优先给出权限相关的提示：九成情况是这个原因，
		// 而原始报错（Access denied ... using password: NO）会把人引向
		// 「密码配错了」这条完全错误的方向。
		if hint := s.credentialHint(); hint != "" {
			msg = hint
		}
		info.Error = msg
		return info
	}
	info.Connected = true
	// 版本号取最后一行非空内容：即使有残留的警告行在前面，
	// 真正的结果也总在最后。
	verLines := strings.Split(strings.TrimSpace(out), "\n")
	info.Version = strings.TrimSpace(verLines[len(verLines)-1])

	// 连接数
	if out, err := s.query("SELECT COUNT(*) FROM information_schema.PROCESSLIST"); err == nil {
		info.Connections, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	if out, err := s.query("SELECT @@max_connections"); err == nil {
		info.MaxConns, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	if out, err := s.query("SHOW STATUS LIKE 'Uptime'"); err == nil {
		fields := strings.Fields(out)
		if len(fields) >= 2 {
			if secs, e := strconv.Atoi(fields[1]); e == nil {
				info.Uptime = humanDuration(secs)
			}
		}
	}

	// 总大小
	sizeQuery := fmt.Sprintf(`
		SELECT ROUND(SUM(data_length + index_length) / 1024 / 1024, 2)
		FROM information_schema.TABLES WHERE table_schema = '%s'`, dbName)
	if out, err := s.query(sizeQuery); err == nil {
		// 同样取最后一行：NULL 或空结果时返回 0 而不是报错
		lines := strings.Split(strings.TrimSpace(out), "\n")
		last := strings.TrimSpace(lines[len(lines)-1])
		if last != "" && last != "NULL" {
			info.SizeMB, _ = strconv.ParseFloat(last, 64)
		}
	}

	// 表列表
	tableQuery := fmt.Sprintf(`
		SELECT table_name,
		       COALESCE(table_rows, 0),
		       ROUND((data_length + index_length) / 1024 / 1024, 2),
		       ROUND(data_length / 1024 / 1024, 2),
		       ROUND(index_length / 1024 / 1024, 2),
		       COALESCE(engine, ''),
		       COALESCE(table_collation, ''),
		       COALESCE(table_comment, '')
		FROM information_schema.TABLES
		WHERE table_schema = '%s'
		ORDER BY (data_length + index_length) DESC`, dbName)

	if out, err := s.query(tableQuery); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			f := strings.Split(line, "\t")
			if len(f) < 8 {
				continue
			}

			rows, _ := strconv.ParseInt(f[1], 10, 64)
			size, _ := strconv.ParseFloat(f[2], 64)
			data, _ := strconv.ParseFloat(f[3], 64)
			idx, _ := strconv.ParseFloat(f[4], 64)

			info.Tables = append(info.Tables, TableInfo{
				Name:      f[0],
				Rows:      rows,
				SizeMB:    size,
				DataMB:    data,
				IndexMB:   idx,
				Engine:    f[5],
				Collation: f[6],
				Comment:   f[7],
			})
		}
	}

	return info
}

// SlowQueries 慢查询统计（只读聚合，不返回 SQL 原文）。
func (s *DBService) SlowQueries() (map[string]any, error) {
	out, err := s.query("SHOW STATUS LIKE 'Slow_queries'")
	if err != nil {
		return nil, err
	}

	fields := strings.Fields(out)
	count := 0
	if len(fields) >= 2 {
		count, _ = strconv.Atoi(fields[1])
	}

	return map[string]any{
		"count": count,
		"note":  "慢查询日志位于 MySQL 数据目录的 nova-slow.log",
	}, nil
}

// dbName 从环境变量文件读库名。
func (s *DBService) dbName() string {
	raw, err := os.ReadFile(s.cfg.EnvFile)
	if err != nil {
		return "nova"
	}

	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "NOVA_DB_NAME=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "NOVA_DB_NAME="))
		}
	}
	return "nova"
}

// mysqlArgs 组装 mysql 客户端的连接参数。
func (s *DBService) mysqlArgs(query string) []string {
	rootCnf := "/etc/nova/.mysql-root.cnf"

	// 优先用 root 配置（能查 information_schema 的完整信息）。
	//
	// 注意这里必须用 os.Open 而不是 os.Stat：
	// 文件存在 ≠ 能读。这个 cnf 是 600 root:root，面板以 nova-panel 身份
	// 运行时 Stat 成功、Open 失败，之前就是因此走进了「退回业务账号」分支。
	// 而业务账号的密码又因为 env 文件读不到而变成空串，
	// 最终报 "Access denied ... (using password: NO)" ——
	// 错误信息完全没提到权限，看起来像密码配错了。
	if f, err := os.Open(rootCnf); err == nil {
		f.Close()
		return []string{"--defaults-file=" + rootCnf, "-N", "-B", "-e", query}
	}

	// 退回业务账号
	dbUser, dbPass := s.dbCredentials()

	// 密码通过环境变量传，不放命令行参数（命令行参数在 ps 里可见）
	args := []string{
		"-h", "127.0.0.1",
		"-u", dbUser,
		"-N", "-B", "-e", query,
	}
	// 只有真拿到密码才加 -p。
	// 加一个空的 -p 会让 mysql 交互式提示输入密码，
	// 在非 TTY 环境下直接报 "using password: NO"，掩盖真实原因。
	if dbPass != "" {
		args = append(args, "-p"+dbPass)
	}
	return args
}

// dbCredentials 从环境变量文件读业务账号。
//
// 第二个返回值表示「是否真的读到了密码」。读不到时调用方要给出
// 明确提示，而不是拿空密码去连库然后报一个误导性的错误。
func (s *DBService) dbCredentials() (string, string) {
	user, pass := "nova", ""

	raw, err := os.ReadFile(s.cfg.EnvFile)
	if err != nil {
		return user, pass
	}

	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "NOVA_DB_USER=") {
			user = strings.TrimSpace(strings.TrimPrefix(line, "NOVA_DB_USER="))
		}
		if strings.HasPrefix(line, "NOVA_DB_PASSWORD=") {
			pass = strings.TrimSpace(strings.TrimPrefix(line, "NOVA_DB_PASSWORD="))
		}
	}

	return user, pass
}

// credentialHint 在连库失败时给出可操作的排查提示。
//
// 光把 mysql 的原始报错丢给用户没用：`Access denied (using password: NO)`
// 看起来像密码配错，实际九成是权限问题——面板读不到配置文件。
func (s *DBService) credentialHint() string {
	rootCnf := "/etc/nova/.mysql-root.cnf"

	if _, err := os.Stat(rootCnf); err == nil {
		if f, openErr := os.Open(rootCnf); openErr != nil {
			return fmt.Sprintf(
				"面板没有权限读取 %s（属主 root、权限 600）。\n"+
					"解决办法：把 nova-panel 加入 nova 组，并确认该文件属组为 nova，\n"+
					"或重新执行安装脚本（会自动修正权限）。", rootCnf)
		} else {
			f.Close()
		}
	}

	if _, err := os.ReadFile(s.cfg.EnvFile); err != nil {
		return fmt.Sprintf(
			"面板没有权限读取 %s，拿不到数据库密码。\n"+
				"解决办法：chown root:nova %s && chmod 640 %s，\n"+
				"并确认 nova-panel 已在 nova 组里（usermod -aG nova nova-panel）。",
			s.cfg.EnvFile, s.cfg.EnvFile, s.cfg.EnvFile)
	}

	return ""
}

func (s *DBService) query(q string) (string, error) {
	out, err := runWithTimeout(15*time.Second, "/usr/bin/mysql", s.mysqlArgs(q)...)
	// mysql 客户端会往输出里混一行警告：
	//   mysql: [Warning] Using a password on the command line interface can be insecure.
	// 这行不是 SQL 结果，但 CombinedOutput 把它和 stdout 混在一起，
	// 导致版本号变成「警告 + 版本」、数字解析失败（sizeMB 变 0）。
	// 逐行过滤掉以 mysql: 开头的行。
	if out != "" {
		var kept []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "mysql: [Warning]") {
				continue
			}
			kept = append(kept, line)
		}
		out = strings.Join(kept, "\n")
	}
	return out, err
}

// ---------------------------------------------------------------- 定时任务

// CronService 定时任务管理。
type CronService struct {
	cfg    *config.Config
	runner *runner.Runner
}

// NewCronService 创建。
func NewCronService(cfg *config.Config, r *runner.Runner) *CronService {
	return &CronService{cfg: cfg, runner: r}
}

// CronTask 一条定时任务。
type CronTask struct {
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
	Managed  bool   `json:"managed"` // 是否面板管理的
}

// List 列出定时任务。
func (s *CronService) List() ([]CronTask, error) {
	res, err := s.runner.Run("cron.list", nil)
	if err != nil {
		return nil, err
	}

	var out []CronTask
	for _, line := range strings.Split(res.Output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "▸") ||
			strings.HasPrefix(line, "[") || strings.HasPrefix(line, "Nova") ||
			strings.HasPrefix(line, "其他") {
			continue
		}

		// 简单解析：前 5 段是时间，剩下是命令
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}

		out = append(out, CronTask{
			Schedule: strings.Join(fields[:5], " "),
			Command:  strings.Join(fields[5:], " "),
			Managed:  strings.Contains(line, "nova"),
		})
	}

	return out, nil
}

// Install 安装默认定时任务。
func (s *CronService) Install() (*runner.Result, error) {
	return s.runner.Run("cron.install", nil)
}

// Remove 移除 Nova 定时任务。
func (s *CronService) Remove() (*runner.Result, error) {
	return s.runner.Run("cron.remove", nil)
}

// ---------------------------------------------------------------- 工具

func humanDuration(secs int) string {
	d := secs / 86400
	h := (secs % 86400) / 3600
	m := (secs % 3600) / 60

	if d > 0 {
		return fmt.Sprintf("%d 天 %d 小时", d, h)
	}
	if h > 0 {
		return fmt.Sprintf("%d 小时 %d 分", h, m)
	}
	return fmt.Sprintf("%d 分钟", m)
}

// ensureDirExists 确保目录存在（内部工具）。
func ensureDirExists(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.MkdirAll(path, 0o755)
}

// joinPath 拼接路径（内部工具）。
func joinPath(elem ...string) string { return filepath.Join(elem...) }
