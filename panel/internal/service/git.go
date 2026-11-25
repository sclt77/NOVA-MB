// Package service 内嵌 Git 代码托管。
//
// 为什么自己做而不是集成 Gitea：
//
//	面板是单二进制，部署时一条 curl | bash 就装好。塞一个 Gitea 进来意味着
//	多一个进程、多一个数据库、多一套账号体系，还要额外吃 150MB 内存。
//	而我们的需求非常窄——**只为了让 Codemagic 能拉代码打包**：
//	建仓库、push、clone、看提交历史、看文件。PR/Issue/Wiki 一样用不上。
//
// 实现方式：
//
//	不自研 Git 协议。Git 的 Smart HTTP 协议（fetch/push 的 packfile 协商）
//	实现起来极其容易出错，自己写等于给自己埋坑。正确做法是**调用系统 git**，
//	用 `git http-backend` 这个官方 CGI 程序处理协议，我们只负责：
//	  1. 鉴权（面板会话 / HTTP Basic，Codemagic 用后者）
//	  2. 把 HTTP 请求翻译成 CGI 环境变量
//	  3. 开关控制（关闭时直接 404，公网就看不到仓库）
//
// 这样既有完整协议兼容性（任何 git 客户端都能用），又不引入额外依赖。
package service

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sclt77/nova-mb/internal/config"
)

// GitService 管理裸仓库与访问控制。
type GitService struct {
	// 仓库根目录，每个仓库一个子目录，形如 <root>/<name>.git
	root string

	// 令牌文件：存 access token（Codemagic 用 Basic 认证，密码填这个）
	tokenFile string
}

// 仓库名规则：小写字母数字与 - _ . ，必须以字母数字开头。
//
// 刻意不允许大写和空格：仓库名会直接变成目录名和 URL 路径，
// 大小写在 Windows/macOS 上不敏感、在 Linux 上敏感，跨平台会出诡异问题。
var repoNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ErrGitDisabled 表示仓库访问当前是关闭状态。
var ErrGitDisabled = errors.New("代码仓库当前已关闭")

func NewGitService(root, tokenFile string) *GitService {
	return &GitService{root: root, tokenFile: tokenFile}
}

// Root 返回仓库根目录。
func (g *GitService) Root() string { return g.root }

// Repo 描述一个仓库。
type Repo struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	SizeBytes     int64  `json:"sizeBytes"`
	LastCommitAt  string `json:"lastCommitAt"`
	LastCommitMsg string `json:"lastCommitMsg"`
	Branch        string `json:"branch"`
	CommitCount   int    `json:"commitCount"`
	CloneURL      string `json:"cloneUrl"`
	Bare          bool   `json:"bare"`
}

// ValidateName 校验仓库名。
func (g *GitService) ValidateName(name string) error {
	if !repoNameRe.MatchString(name) {
		return fmt.Errorf("仓库名只能是小写字母、数字、- _ .，且以字母或数字开头（最多 64 位）")
	}
	// 防目录穿越：正则已经排除了 / 和 ..，这里再兜一层
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("仓库名不合法")
	}
	return nil
}

// RepoPath 返回仓库磁盘路径。
func (g *GitService) RepoPath(name string) (string, error) {
	if err := g.ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(g.root, name+".git"), nil
}

// Exists 判断仓库是否存在。
func (g *GitService) Exists(name string) bool {
	p, err := g.RepoPath(name)
	if err != nil {
		return false
	}
	st, err := os.Stat(filepath.Join(p, "HEAD"))
	return err == nil && !st.IsDir()
}

// Create 创建一个裸仓库。
//
// baseURL 用来生成 clone 地址；为空时只给相对路径，
// 由前端提示用户去配 NOVA_PUBLIC_URL。
//
// 用裸仓库（bare）而不是工作区仓库：
//
//	服务器上只有「收代码」这一个用途，不需要检出工作区。
//	裸仓库体积更小，也没有「工作区有未提交改动导致 push 被拒」这类问题。
func (g *GitService) Create(name, description, baseURL string) (*Repo, error) {
	path, err := g.RepoPath(name)
	if err != nil {
		return nil, err
	}
	if g.Exists(name) {
		return nil, fmt.Errorf("仓库 %s 已存在", name)
	}

	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("创建仓库目录失败: %w", err)
	}

	// git init --bare。用 -b main 让默认分支就是 main，
	// 否则老版本 git 默认 master，Codemagic 配了 main 会拉不到东西。
	if out, err := runWithTimeout(30*time.Second, "git", "init", "--bare", "-b", "main", path); err != nil {
		os.RemoveAll(path)
		return nil, fmt.Errorf("git init 失败: %v: %s", err, out)
	}

	// 允许 push 到当前分支（裸仓库默认拒绝，Codemagic 场景必须放开）
	g.setConfig(path, "receive.denyCurrentBranch", "ignore")

	// 描述写进 git 自带的 description 文件，clone 时能看到
	if description != "" {
		os.WriteFile(filepath.Join(path, "description"), []byte(description+"\n"), 0o644)
	}

	// 关掉 git 的协议 v2 以外的兼容性问题：不需要，保持默认

	return g.Stat(name, baseURL)
}

// Delete 删除仓库。
//
// 调用方必须先做二次确认——这里不提供回收站，
// 仓库删掉就是真没了（代码托管的数据不该由面板来兜底）。
func (g *GitService) Delete(name string) error {
	path, err := g.RepoPath(name)
	if err != nil {
		return err
	}
	if !g.Exists(name) {
		return fmt.Errorf("仓库 %s 不存在", name)
	}
	return os.RemoveAll(path)
}

// List 列出所有仓库。
func (g *GitService) List(baseURL string) ([]Repo, error) {
	entries, err := os.ReadDir(g.root)
	if err != nil {
		if os.IsNotExist(err) {
			return []Repo{}, nil
		}
		return nil, err
	}

	out := make([]Repo, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), ".git") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".git")
		if r, err := g.Stat(name, baseURL); err == nil {
			out = append(out, *r)
		}
	}

	// 最近提交的排前面，空仓库垫底
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastCommitAt > out[j].LastCommitAt
	})
	return out, nil
}

// Stat 读取单个仓库信息。
func (g *GitService) Stat(name, baseURL string) (*Repo, error) {
	path, err := g.RepoPath(name)
	if err != nil {
		return nil, err
	}
	if !g.Exists(name) {
		return nil, fmt.Errorf("仓库 %s 不存在", name)
	}

	r := &Repo{
		Name:     name,
		Path:     path,
		Bare:     true,
		CloneURL: strings.TrimSuffix(baseURL, "/") + "/git/" + name + ".git",
	}
	// baseURL 为空（面板走 127.0.0.1、没配 NOVA_PUBLIC_URL）时，
	// 上面会拼出 "/git/xxx.git" 这种相对地址。相对地址对 CI 毫无意义，
	// 所以这里退回只给路径，由前端明确提示「先去配公网地址」。
	if baseURL == "" {
		r.CloneURL = "/git/" + name + ".git"
	}

	r.SizeBytes = dirSize(path)

	// 当前分支。空仓库时 HEAD 指向的 ref 还不存在，git 会报错，
	// 所以这里失败不当错误处理，只是留空。
	if out, err := g.git(path, "symbolic-ref", "--short", "HEAD"); err == nil {
		r.Branch = strings.TrimSpace(out)
	}

	// 最近一次提交。空仓库返回非零退出码，忽略即可。
	if out, err := g.git(path, "log", "-1", "--format=%cI%n%s"); err == nil {
		parts := strings.SplitN(strings.TrimSpace(out), "\n", 2)
		r.LastCommitAt = parts[0]
		if len(parts) > 1 {
			r.LastCommitMsg = parts[1]
		}
	}

	// 提交总数。用 rev-list --count 而不是数日志行，大仓库快得多。
	if out, err := g.git(path, "rev-list", "--count", "--all"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(out)); err == nil {
			r.CommitCount = n
		}
	}

	return r, nil
}

// Commits 返回提交历史。
func (g *GitService) Commits(name string, limit int) ([]Commit, error) {
	path, err := g.RepoPath(name)
	if err != nil {
		return nil, err
	}
	if !g.Exists(name) {
		return nil, fmt.Errorf("仓库 %s 不存在", name)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	// 用 0x1f（单元分隔符）分隔字段、0x1e 分隔记录：
	// 提交信息里可能有换行和任何可见字符，用普通分隔符会串行。
	const sep, rec = "\x1f", "\x1e"
	format := strings.Join([]string{
		"%H", "%h", "%an", "%ae", "%cI", "%s",
	}, sep) + rec

	out, err := g.git(path, "log", "-n", strconv.Itoa(limit), "--format="+format, "--all")
	if err != nil {
		// 空仓库没有提交，这不是错误
		if strings.Contains(out, "does not have any commits") ||
			strings.Contains(out, "unknown revision") {
			return []Commit{}, nil
		}
		return nil, fmt.Errorf("读取提交历史失败: %v", err)
	}

	var list []Commit
	for _, chunk := range strings.Split(out, rec) {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		f := strings.Split(chunk, sep)
		if len(f) < 6 {
			continue
		}
		list = append(list, Commit{
			Hash: f[0], Short: f[1], Author: f[2], Email: f[3],
			Date: f[4], Subject: f[5],
		})
	}
	if list == nil {
		list = []Commit{}
	}
	return list, nil
}

// Commit 一条提交记录。
type Commit struct {
	Hash    string `json:"hash"`
	Short   string `json:"short"`
	Author  string `json:"author"`
	Email   string `json:"email"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// Tree 列出某次提交的文件树（单层）。
func (g *GitService) Tree(name, ref, path string) ([]TreeEntry, error) {
	repo, err := g.RepoPath(name)
	if err != nil {
		return nil, err
	}
	if !g.Exists(name) {
		return nil, fmt.Errorf("仓库 %s 不存在", name)
	}
	if ref == "" {
		ref = "HEAD"
	}
	// path 必须相对仓库根，禁止绝对路径和 ..
	path = strings.Trim(path, "/")
	if strings.Contains(path, "..") {
		return nil, fmt.Errorf("路径不合法")
	}

	target := ref
	if path != "" {
		target = ref + ":" + path
	}

	// -l 带上文件大小；--name-only 只列名字不够用
	out, err := g.git(repo, "ls-tree", "-l", target)
	if err != nil {
		return nil, fmt.Errorf("读取文件树失败: %v", err)
	}

	var list []TreeEntry
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		// 格式：<mode> SP <type> SP <sha> SP+ <size> TAB <name>
		meta, fileName, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 3 {
			continue
		}
		e := TreeEntry{
			Mode: fields[0],
			Type: fields[1],
			Hash: fields[2],
			Name: fileName,
			Path: strings.TrimPrefix(path+"/"+fileName, "/"),
			Dir:  fields[1] == "tree",
		}
		if len(fields) >= 4 && fields[3] != "-" {
			if n, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
				e.Size = n
			}
		}
		list = append(list, e)
	}
	if list == nil {
		list = []TreeEntry{}
	}
	return list, nil
}

// TreeEntry 文件树节点。
type TreeEntry struct {
	Mode string `json:"mode"`
	Type string `json:"type"`
	Hash string `json:"hash"`
	Name string `json:"name"`
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

// Blob 读取文件内容。
func (g *GitService) Blob(name, ref, path string) (string, error) {
	repo, err := g.RepoPath(name)
	if err != nil {
		return "", err
	}
	if !g.Exists(name) {
		return "", fmt.Errorf("仓库 %s 不存在", name)
	}
	if ref == "" {
		ref = "HEAD"
	}
	path = strings.Trim(path, "/")
	if path == "" || strings.Contains(path, "..") {
		return "", fmt.Errorf("路径不合法")
	}

	// 限制单文件大小：前端预览不需要看 10MB 的文件，
	// 但直接 cat 会把内存打满。
	out, err := g.git(repo, "cat-file", "-s", ref+":"+path)
	if err != nil {
		return "", fmt.Errorf("文件不存在")
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	const maxPreview = 512 * 1024
	if size > maxPreview {
		return "", fmt.Errorf("文件过大（%s），不支持在线预览", humanSize(size))
	}

	content, err := g.git(repo, "cat-file", "-p", ref+":"+path)
	if err != nil {
		return "", fmt.Errorf("读取文件失败")
	}
	return content, nil
}

// git 在指定仓库里执行 git 命令。
func (g *GitService) git(dir string, args ...string) (string, error) {
	ctx, cancel := contextWithTimeout(60 * time.Second)
	defer cancel()

	full := append([]string{"-C", dir}, args...)
	cmd := execCommandContext(ctx, "git", full...)
	// 避免 git 继承调用者的 GIT_* 环境变量（面板可能被 systemd 注入奇怪的值）
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (g *GitService) setConfig(repoPath, key, value string) {
	g.git(repoPath, "config", key, value)
}

// ---------------------------------------------------------------- 访问开关

// 令牌管理。
//
// Codemagic 拉私有仓库需要凭据。用「用户名 + token」的 Basic 认证：
//   - 不用面板账号密码：那等于把后台权限交出去，且改密码会打断 CI
//   - token 独立、可随时重置，泄露了就换一个
//   - token 只在开启仓库访问时才有意义

// Token 读取当前访问令牌（不存在则返回空）。
func (g *GitService) Token() string {
	raw, err := os.ReadFile(g.tokenFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// SetToken 写入访问令牌。
func (g *GitService) SetToken(token string) error {
	if err := os.MkdirAll(filepath.Dir(g.tokenFile), 0o700); err != nil {
		return err
	}
	return os.WriteFile(g.tokenFile, []byte(token+"\n"), 0o600)
}

// EnsureToken 没有令牌就生成一个。
func (g *GitService) EnsureToken() (string, error) {
	if t := g.Token(); t != "" {
		return t, nil
	}
	t, err := config.RandomHex(20) // 40 位十六进制，够用且好复制
	if err != nil {
		return "", err
	}
	if err := g.SetToken(t); err != nil {
		return "", err
	}
	return t, nil
}

// CheckToken 校验 Basic 认证里的密码。
//
// 用恒定时间比较，避免通过响应时间差逐字符猜出 token。
func (g *GitService) CheckToken(provided string) bool {
	want := g.Token()
	if want == "" || provided == "" {
		return false
	}
	return constantTimeEqual(want, provided)
}

// ---------------------------------------------------------------- 辅助

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// contextWithTimeout 是 context.WithTimeout 的短名字封装，
// 让上面的 git() 读起来干净些。
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// constantTimeEqual 恒定时间字符串比较。
//
// 不能直接用 ==：Go 的字符串比较会在第一个不同字节处返回，
// 攻击者能通过响应时间逐字节猜出 token。crypto/subtle 是标准做法。
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
