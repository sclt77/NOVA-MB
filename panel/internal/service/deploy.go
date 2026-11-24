package service

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sclt77/nova-mb/internal/config"
	"github.com/sclt77/nova-mb/internal/runner"
)

// DeployService 部署管理。
type DeployService struct {
	cfg    *config.Config
	runner *runner.Runner
}

// NewDeployService 创建。
func NewDeployService(cfg *config.Config, r *runner.Runner) *DeployService {
	return &DeployService{cfg: cfg, runner: r}
}

// Release 一个已部署的版本。
type Release struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"createdAt"`
	Current   bool      `json:"current"`
	HasWeb    bool      `json:"hasWeb"`
	JarSize   int64     `json:"jarSize"`
}

// Releases 列出历史版本。
func (s *DeployService) Releases() ([]Release, error) {
	entries, err := os.ReadDir(s.cfg.ReleasesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Release{}, nil
		}
		return nil, err
	}

	// 当前版本通过软链解析
	current := ""
	if target, err := os.Readlink(filepath.Join(s.cfg.NovaAppDir, "current")); err == nil {
		current = filepath.Base(target)
	}

	var out []Release
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		name := e.Name()
		dir := filepath.Join(s.cfg.ReleasesDir, name)

		rel := Release{
			Name:    name,
			Current: name == current,
		}

		if st, err := os.Stat(dir); err == nil {
			rel.CreatedAt = st.ModTime()
		}

		// 统计大小
		rel.Size = dirSize(dir)

		// jar 大小
		if st, err := os.Stat(filepath.Join(dir, "nova-api.jar")); err == nil {
			rel.JarSize = st.Size()
		}

		// 是否含前端
		if st, err := os.Stat(filepath.Join(dir, "web", "index.html")); err == nil && !st.IsDir() {
			rel.HasWeb = true
		}

		out = append(out, rel)
	}

	// 新的在前
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name > out[j].Name
	})

	return out, nil
}

// UploadResult 上传结果。
type UploadResult struct {
	Path      string   `json:"path"`
	Size      int64    `json:"size"`
	Warnings  []string `json:"warnings"`
	Validated bool     `json:"validated"`
}

// UploadPackage 保存并校验上传的部署包。
//
// 校验在保存之后、部署之前做：包结构不对就立刻告诉用户，
// 不要等到部署到一半才发现缺文件。
func (s *DeployService) UploadPackage(filename string, src io.Reader) (*UploadResult, error) {
	// 上传目录
	uploadRoot := filepath.Join(s.cfg.DataDir, "packages")
	if err := os.MkdirAll(uploadRoot, 0o700); err != nil {
		return nil, fmt.Errorf("创建上传目录失败: %w", err)
	}

	// 文件名由服务端生成，不使用用户提供的名字——避免路径穿越
	stamp := time.Now().Format("20060102-150405")
	dest := filepath.Join(uploadRoot, "deploy-"+stamp+".zip")

	f, err := os.Create(dest)
	if err != nil {
		return nil, fmt.Errorf("创建文件失败: %w", err)
	}

	// 限制大小，防止磁盘被写满
	limited := io.LimitReader(src, s.cfg.MaxUploadBytes+1)
	written, err := io.Copy(f, limited)
	f.Close()

	if err != nil {
		os.Remove(dest)
		return nil, fmt.Errorf("写入文件失败: %w", err)
	}

	if written > s.cfg.MaxUploadBytes {
		os.Remove(dest)
		return nil, fmt.Errorf("包超过大小上限（%d MB）", s.cfg.MaxUploadBytes/1024/1024)
	}

	res := &UploadResult{Path: dest, Size: written}

	// 校验结构
	warnings, err := validateZip(dest)
	if err != nil {
		os.Remove(dest)
		return nil, err
	}

	res.Warnings = warnings
	res.Validated = true

	return res, nil
}

// validateZip 校验部署包结构。
//
// 与 deploy.sh 里的检查保持一致，在面板层先拦一次，
// 用户上传完立刻知道包对不对，不用等部署失败。
func validateZip(path string) ([]string, error) {
	r, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("压缩包已损坏或不是 zip 格式")
	}
	defer r.Close()

	var hasJar, hasIndex bool
	var warnings []string
	var totalSize int64

	for _, f := range r.File {
		name := f.Name

		// 路径安全
		if strings.HasPrefix(name, "/") {
			return nil, fmt.Errorf("包内含绝对路径：%s", name)
		}
		if strings.Contains("/"+name+"/", "/../") {
			return nil, fmt.Errorf("包内含路径穿越：%s", name)
		}

		// 单个文件不超过 300MB
		if f.UncompressedSize64 > 300*1024*1024 {
			return nil, fmt.Errorf("包内文件过大：%s", name)
		}
		totalSize += int64(f.UncompressedSize64)

		base := filepath.Base(name)
		switch {
		case base == "nova-api.jar":
			hasJar = true
		case strings.HasSuffix(name, "web/index.html"):
			hasIndex = true
		}
	}

	if !hasJar {
		return nil, fmt.Errorf("包内缺少 nova-api.jar")
	}
	if !hasIndex {
		return nil, fmt.Errorf("包内缺少 web/index.html")
	}

	// 解压后超过 2GB 直接拒绝，防 zip 炸弹
	if totalSize > 2*1024*1024*1024 {
		return nil, fmt.Errorf("包解压后过大（%d MB），疑似异常", totalSize/1024/1024)
	}

	if totalSize > 500*1024*1024 {
		warnings = append(warnings,
			fmt.Sprintf("包较大（%d MB），部署会比较慢", totalSize/1024/1024))
	}

	return warnings, nil
}

// Deploy 执行部署。
func (s *DeployService) Deploy(pkgPath string) (*runner.Result, error) {
	// 只允许部署上传目录里的包，不接受任意路径
	uploadRoot := filepath.Join(s.cfg.DataDir, "packages")
	abs, err := filepath.Abs(pkgPath)
	if err != nil {
		return nil, fmt.Errorf("路径不合法")
	}
	if !strings.HasPrefix(abs, uploadRoot+string(filepath.Separator)) {
		return nil, fmt.Errorf("只能部署上传目录中的包")
	}

	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("包不存在")
	}

	return s.runner.Run("deploy", map[string]string{"package": abs})
}

// Rollback 回滚到上一个版本。
func (s *DeployService) Rollback() (*runner.Result, error) {
	return s.runner.Run("rollback", nil)
}

// CleanPackages 清理上传的旧包（部署完成后调用）。
func (s *DeployService) CleanPackages(keep int) {
	uploadRoot := filepath.Join(s.cfg.DataDir, "packages")
	entries, err := os.ReadDir(uploadRoot)
	if err != nil {
		return
	}

	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".zip") {
			files = append(files, e.Name())
		}
	}

	if len(files) <= keep {
		return
	}

	sort.Strings(files)
	for _, name := range files[:len(files)-keep] {
		os.Remove(filepath.Join(uploadRoot, name))
	}
}

// dirSize 递归统计目录大小。
func dirSize(path string) int64 {
	var total int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// ---------------------------------------------------------------- 备份

// BackupService 备份管理。
type BackupService struct {
	cfg    *config.Config
	runner *runner.Runner
}

// NewBackupService 创建。
func NewBackupService(cfg *config.Config, r *runner.Runner) *BackupService {
	return &BackupService{cfg: cfg, runner: r}
}

// Backup 一份备份。
type Backup struct {
	Stamp     string    `json:"stamp"`
	DBSize    int64     `json:"dbSize"`
	Uploads   int64     `json:"uploads"`
	HasUpload bool      `json:"hasUpload"`
	CreatedAt time.Time `json:"createdAt"`
}

// 备份文件名：db-20261124-103000.sql.gz
var reBackupFile = regexp.MustCompile(`^db-(\d{8}-\d{6})\.sql\.gz$`)

// List 列出备份。
func (s *BackupService) List() ([]Backup, error) {
	entries, err := os.ReadDir(s.cfg.BackupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Backup{}, nil
		}
		return nil, err
	}

	var out []Backup
	for _, e := range entries {
		if e.IsDir() {
			continue
		}

		m := reBackupFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}

		b := Backup{Stamp: m[1]}

		if st, err := e.Info(); err == nil {
			b.DBSize = st.Size()
			b.CreatedAt = st.ModTime()
		}

		// 对应的 uploads 包
		upName := "uploads-" + b.Stamp + ".tar.gz"
		if st, err := os.Stat(filepath.Join(s.cfg.BackupDir, upName)); err == nil {
			b.Uploads = st.Size()
			b.HasUpload = true
		}

		out = append(out, b)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Stamp > out[j].Stamp
	})

	return out, nil
}

// RunBackup 执行备份。
func (s *BackupService) RunBackup(full bool) (*runner.Result, error) {
	if full {
		return s.runner.Run("backup.full", nil)
	}
	return s.runner.Run("backup.quick", nil)
}

// Verify 验证最近一次备份。
func (s *BackupService) Verify() (*runner.Result, error) {
	return s.runner.Run("backup.verify", nil)
}

// Restore 从备份恢复。
func (s *BackupService) Restore(stamp, scope string) (*runner.Result, error) {
	if !runner.ValidStamp(stamp) {
		return nil, fmt.Errorf("备份时间戳不合法")
	}
	return s.runner.Run("restore", map[string]string{"stamp": stamp, "scope": scope})
}

// DeleteBackup 删除一份备份。
func (s *BackupService) DeleteBackup(stamp string) error {
	if !runner.ValidStamp(stamp) {
		return fmt.Errorf("备份时间戳不合法")
	}

	dbFile := filepath.Join(s.cfg.BackupDir, "db-"+stamp+".sql.gz")
	upFile := filepath.Join(s.cfg.BackupDir, "uploads-"+stamp+".tar.gz")
	mfFile := filepath.Join(s.cfg.BackupDir, "manifest-"+stamp+".txt")

	// 必须确认文件在备份目录内（防路径穿越）
	for _, f := range []string{dbFile, upFile, mfFile} {
		abs, err := filepath.Abs(f)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(abs, s.cfg.BackupDir) {
			return fmt.Errorf("路径不合法")
		}
		os.Remove(abs)
	}

	return nil
}

// TotalSize 备份总占用。
func (s *BackupService) TotalSize() int64 {
	return dirSize(s.cfg.BackupDir)
}
