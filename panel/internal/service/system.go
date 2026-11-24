// Package service 面板业务逻辑。
//
// 这一层负责：读系统状态、生成配置、调用 runner 执行动作。
// 所有用户输入在这里完成校验后才传给 runner。
package service

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sclt77/nova-mb/internal/config"
)

// SystemService 系统状态采集。
type SystemService struct {
	cfg *config.Config
}

// NewSystemService 创建。
func NewSystemService(cfg *config.Config) *SystemService {
	return &SystemService{cfg: cfg}
}

// ServiceStatus 单个服务的状态。
type ServiceStatus struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Active  bool   `json:"active"`
	State   string `json:"state"`
	Enabled bool   `json:"enabled"`
}

// Overview 概览数据。
type Overview struct {
	Hostname   string          `json:"hostname"`
	OS         string          `json:"os"`
	Kernel     string          `json:"kernel"`
	Uptime     string          `json:"uptime"`
	CPUCores   int             `json:"cpuCores"`
	LoadAvg    []float64       `json:"loadAvg"`
	MemTotalMB int64           `json:"memTotalMB"`
	MemUsedMB  int64           `json:"memUsedMB"`
	MemPercent int             `json:"memPercent"`
	DiskTotalMB int64          `json:"diskTotalMB"`
	DiskUsedMB  int64          `json:"diskUsedMB"`
	DiskPercent int            `json:"diskPercent"`
	DiskWarn    bool           `json:"diskWarn"`
	DiskCrit    bool           `json:"diskCrit"`
	Services   []ServiceStatus `json:"services"`
	Version    string          `json:"version"`
	GoVersion  string          `json:"goVersion"`
	ServerTime string          `json:"serverTime"`
	Warnings   []string        `json:"warnings"`
}

// CollectOverview 采集概览。
func (s *SystemService) CollectOverview() *Overview {
	ov := &Overview{
		GoVersion:  runtime.Version(),
		ServerTime: time.Now().Format("2006-01-02 15:04:05"),
	}

	ov.Hostname, _ = os.Hostname()

	// 系统信息
	if raw, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				ov.OS = strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
				break
			}
		}
	}
	if raw, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		ov.Kernel = strings.TrimSpace(string(raw))
	}

	ov.CPUCores = runtime.NumCPU()
	ov.Uptime = readUptime()
	ov.LoadAvg = readLoadAvg()
	ov.MemTotalMB, ov.MemUsedMB, ov.MemPercent = readMemory()

	ov.DiskTotalMB, ov.DiskUsedMB, ov.DiskPercent = readDisk("/var")
	ov.DiskWarn = ov.DiskPercent >= 80
	ov.DiskCrit = ov.DiskPercent >= 90

	// 服务状态
	ov.Services = []ServiceStatus{
		s.serviceStatus(s.cfg.APIService, "Nova 后端"),
		s.serviceStatus(s.cfg.NginxService, "Nginx"),
		s.serviceStatus(mysqlUnitName(), "MySQL"),
		s.serviceStatus("nova-panel", "面板"),
	}

	// 警告
	if ov.DiskCrit {
		ov.Warnings = append(ov.Warnings,
			fmt.Sprintf("磁盘使用率 %d%%，已超过 90%%，请立即清理", ov.DiskPercent))
	} else if ov.DiskWarn {
		ov.Warnings = append(ov.Warnings,
			fmt.Sprintf("磁盘使用率 %d%%，超过 80%% 告警线", ov.DiskPercent))
	}

	for _, svc := range ov.Services {
		if !svc.Active && svc.Name != "nova-panel" {
			ov.Warnings = append(ov.Warnings, svc.Label+" 未运行")
		}
	}

	if ov.MemPercent >= 90 {
		ov.Warnings = append(ov.Warnings,
			fmt.Sprintf("内存使用率 %d%%，可能影响服务稳定性", ov.MemPercent))
	}

	return ov
}

func (s *SystemService) serviceStatus(name, label string) ServiceStatus {
	st := ServiceStatus{Name: name, Label: label}

	out, err := runQuick("/bin/systemctl", "is-active", name)
	st.State = strings.TrimSpace(out)
	st.Active = err == nil && st.State == "active"

	out2, err2 := runQuick("/bin/systemctl", "is-enabled", name)
	st.Enabled = err2 == nil && strings.TrimSpace(out2) == "enabled"

	if st.State == "" {
		st.State = "unknown"
	}

	return st
}

// ---------------------------------------------------------------- 系统指标

func readUptime() string {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return "未知"
	}

	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return "未知"
	}

	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "未知"
	}

	d := int(secs) / 86400
	h := (int(secs) % 86400) / 3600
	m := (int(secs) % 3600) / 60

	if d > 0 {
		return fmt.Sprintf("%d 天 %d 小时", d, h)
	}
	if h > 0 {
		return fmt.Sprintf("%d 小时 %d 分", h, m)
	}
	return fmt.Sprintf("%d 分钟", m)
}

func readLoadAvg() []float64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return []float64{0, 0, 0}
	}

	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return []float64{0, 0, 0}
	}

	out := make([]float64, 3)
	for i := 0; i < 3; i++ {
		out[i], _ = strconv.ParseFloat(fields[i], 64)
	}
	return out
}

func readMemory() (totalMB, usedMB int64, percent int) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0
	}

	var totalKB, availKB int64
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			totalKB = v
		case "MemAvailable:":
			availKB = v
		}
	}

	totalMB = totalKB / 1024
	if totalKB > 0 {
		usedMB = (totalKB - availKB) / 1024
		percent = int((totalKB - availKB) * 100 / totalKB)
	}
	return
}

// ---------------------------------------------------------------- 日志

// LogService 日志查看。
type LogService struct {
	cfg *config.Config
}

// NewLogService 创建。
func NewLogService(cfg *config.Config) *LogService {
	return &LogService{cfg: cfg}
}

// LogFile 日志文件描述。
type LogFile struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Path  string `json:"-"`
	Size  int64  `json:"size"`
}

// Files 可查看的日志列表。
//
// 白名单固定，不接受用户传路径——否则就是任意文件读取漏洞。
func (s *LogService) Files() []LogFile {
	candidates := []LogFile{
		{Key: "nova-api.log", Label: "应用日志", Path: filepath.Join(s.cfg.LogDir, "nova-api.log")},
		{Key: "install.log", Label: "安装日志", Path: filepath.Join(s.cfg.LogDir, "install.log")},
		{Key: "cron.log", Label: "定时任务日志", Path: filepath.Join(s.cfg.LogDir, "cron.log")},
		{Key: "panel", Label: "面板日志", Path: filepath.Join(s.cfg.LogDir, "panel.log")},
		{Key: "nginx-error", Label: "Nginx 错误日志", Path: "/var/log/nginx/error.log"},
		{Key: "nginx-access", Label: "Nginx 访问日志", Path: "/var/log/nginx/access.log"},
	}

	out := make([]LogFile, 0, len(candidates))
	for _, f := range candidates {
		if st, err := os.Stat(f.Path); err == nil {
			f.Size = st.Size()
			out = append(out, f)
		} else {
			// 文件不存在也列出来，让用户知道有这一项（大小 0）
			out = append(out, f)
		}
	}
	return out
}

// Tail 读日志尾部。
//
// maxLines 限制在 2000 行内：日志文件可能几百 MB，
// 全读进内存会把面板自己搞崩。
func (s *LogService) Tail(key string, maxLines int) (string, error) {
	var path string
	found := false
	for _, f := range s.Files() {
		if f.Key == key {
			path = f.Path
			found = true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("未知日志")
	}

	if maxLines <= 0 || maxLines > 2000 {
		maxLines = 500
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("无法打开日志文件")
	}
	defer f.Close()

	// 只读尾部，不整个文件读进内存
	st, err := f.Stat()
	if err != nil {
		return "", err
	}

	const chunkSize = 256 * 1024
	offset := st.Size() - chunkSize
	if offset < 0 {
		offset = 0
	}

	if _, err := f.Seek(offset, 0); err != nil {
		return "", err
	}

	var lines []string
	sc := bufio.NewScanner(f)
	// 单行可能很长（比如堆栈），放宽限制
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	for sc.Scan() {
		lines = append(lines, sc.Text())
	}

	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}

	// 反转让最新的在最上面
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}

	return strings.Join(lines, "\n"), nil
}

// ---------------------------------------------------------------- 工具

// mysqlUnitName 返回 MySQL 的服务名（Debian 系 mysql，RHEL 系 mysqld）。
func mysqlUnitName() string {
	if _, err := os.Stat("/etc/systemd/system/mysqld.service"); err == nil {
		return "mysqld"
	}
	if out, err := runQuick("/bin/systemctl", "list-unit-files", "--type=service", "--no-legend"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "mysqld.service") {
				return "mysqld"
			}
		}
	}
	return "mysql"
}

// runQuick 快速执行命令（内部使用，不经 runner 白名单）。
//
// 仅用于只读的状态查询，命令与参数都是代码里写死的常量。
// 任何涉及用户输入的命令都必须走 runner。
func runQuick(bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := execCommandContext(ctx, bin, args...)
	out, err := cmd.Output()
	return string(out), err
}

// sortStrings 排序（供上层稳定输出用）。
func sortStrings(s []string) { sort.Strings(s) }
