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
		info.Error = "无法连接数据库：" + strings.TrimSpace(out)
		return info
	}
	info.Connected = true
	info.Version = strings.TrimSpace(out)

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
		info.SizeMB, _ = strconv.ParseFloat(strings.TrimSpace(out), 64)
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

	// 优先用 root 配置（能查 information_schema 的完整信息）
	if _, err := os.Stat(rootCnf); err == nil {
		return []string{"--defaults-file=" + rootCnf, "-N", "-B", "-e", query}
	}

	// 退回业务账号
	dbUser, dbPass := s.dbCredentials()

	// 密码通过环境变量传，不放命令行参数（命令行参数在 ps 里可见）
	return []string{
		"-h", "127.0.0.1",
		"-u", dbUser,
		"-p" + dbPass,
		"-N", "-B", "-e", query,
	}
}

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

func (s *DBService) query(q string) (string, error) {
	return runWithTimeout(15*time.Second, "/usr/bin/mysql", s.mysqlArgs(q)...)
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
