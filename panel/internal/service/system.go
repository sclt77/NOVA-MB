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
		{Key: "nova-api", Label: "应用日志", Path: ""}, // 走 journald，见 Tail
		{Key: "install.log", Label: "安装日志", Path: filepath.Join(s.cfg.LogDir, "install.log")},
		{Key: "cron.log", Label: "定时任务日志", Path: filepath.Join(s.cfg.LogDir, "cron.log")},
		{Key: "panel", Label: "面板日志", Path: ""}, // 同样走 journald
		{Key: "nginx-error", Label: "Nginx 错误日志", Path: "/var/log/nginx/error.log"},
		{Key: "nginx-access", Label: "Nginx 访问日志", Path: "/var/log/nginx/access.log"},
	}

	out := make([]LogFile, 0, len(candidates))
	for _, f := range candidates {
		if f.Path == "" {
			// journald 来源：大小无从得知，给个 0 让前端照常渲染
			out = append(out, f)
			continue
		}
		if st, err := os.Stat(f.Path); err == nil {
			f.Size = st.Size()
		}
		// 文件不存在也列出来（大小 0），前端会提示「暂无日志」
		out = append(out, f)
	}
	return out
}

// journaldUnits 日志键 → systemd 单元名。
//
// 为什么不写日志文件：
//
//	nova-api 与 nova-panel 都由 systemd 托管，输出默认进 journald。
//	之前这里假设存在 /var/log/nova/nova-api.log，但那个文件根本不会被创建，
//	于是「日志」页永远显示「读取失败：无法打开日志文件」。
//	正确做法是直接读 journald，而不是再配一套 logback 文件输出——
//	多一份日志文件就多一处轮转、权限、磁盘占用的负担。
var journaldUnits = map[string]string{
	"nova-api": "nova-api",
	"panel":    "nova-panel",
}

// Tail 读日志尾部。
//
// maxLines 限制在 2000 行内：日志文件可能几百 MB，
// 全读进内存会把面板自己搞崩。
func (s *LogService) Tail(key string, maxLines int) (string, error) {
	if maxLines <= 0 || maxLines > 2000 {
		maxLines = 500
	}

	// journald 来源优先
	if unit, ok := journaldUnits[key]; ok {
		return s.tailJournal(unit, maxLines)
	}

	var path string
	found := false
	for _, f := range s.Files() {
		if f.Key == key {
			path = f.Path
			found = true
			break
		}
	}
	if !found || path == "" {
		return "", fmt.Errorf("未知日志")
	}

	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("日志文件不存在：%s\n（该服务可能未运行，或日志尚未产生）", path)
	}

	f, err := os.Open(path)
	if err != nil {
		// 区分「不存在」和「没权限」——两者的解决办法完全不同
		if os.IsPermission(err) {
			return "", fmt.Errorf("没有权限读取 %s\n"+
				"面板以 nova-panel 身份运行，需要读该文件的组权限。", path)
		}
		return "", fmt.Errorf("无法打开日志文件：%v", err)
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

// tailJournal 从 journald 读某个单元的日志尾部。
//
// 用 journalctl 而不是读 /var/log/journal 的二进制文件：
// journald 的存储格式是私有的、带索引的，自己解析既脆弱又没必要。
//
// 参数说明：
//   -n N           取最近 N 行
//   --no-pager     不要调用 less（非 TTY 下会卡住）
//   -o short-iso   时间戳用 ISO 格式，前端好按时间排序
//   --no-hostname  去掉主机名，省一行宽度
func (s *LogService) tailJournal(unit string, maxLines int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := execCommandContext(ctx, "/usr/bin/journalctl",
		"-u", unit,
		"-n", strconv.Itoa(maxLines),
		"--no-pager",
		"--no-hostname",
		"-o", "short-iso",
	)
	// journalctl 读系统日志需要 systemd-journal 组权限。
	// 面板用户不在该组时会报 "No journal files were found"，
	// 这里把 stderr 一起收回来，好给用户看真实原因。
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))

	if err != nil {
		if strings.Contains(text, "No journal files") ||
			strings.Contains(text, "not seeing messages") {
			return "", fmt.Errorf(
				"面板没有权限读取系统日志。\n"+
					"解决办法：usermod -aG systemd-journal nova-panel 后重启面板。\n"+
					"（安装脚本会自动处理；如果手动改过用户组，需要重新登录生效）")
		}
		return "", fmt.Errorf("读取 journald 失败：%s", text)
	}

	if text == "" {
		return "（暂无日志）", nil
	}

	// journalctl 已经按时间正序输出（旧→新），
	// 但面板其他日志都是「最新的在最上面」，这里统一反转。
	lines := strings.Split(text, "\n")
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "\n"), nil
}

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
