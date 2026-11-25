package service

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// GitSmartHTTP 处理 Git 的 Smart HTTP 协议。
//
// 为什么不自己实现协议：
//
//	Git 的 fetch/push 是 packfile 协商，涉及 want/have 列表、能力协商、
//	side-band 多路复用、浅克隆等一堆细节。自己写必然在某个边界条件下出错，
//	而且出错方式是「clone 卡住不动」或「push 后对象损坏」，极难排查。
//
// 正确做法是用 git 官方的 `git http-backend`（CGI 程序）：
//
//	它由 git 官方维护，兼容所有客户端（git CLI / libgit2 / Codemagic / JGit）。
//	我们只负责把 HTTP 请求翻译成 CGI 环境变量，再把 CGI 输出翻译回 HTTP 响应。
//	这是 GitHub/Gitea/GitLab 之外无数自建 git 服务的标准做法。
//
// 安全性：路径完全由我们控制（仓库名先过 ValidateName 正则），
// 再拼成 GIT_PROJECT_ROOT + PATH_INFO，不存在目录穿越。

// smartHTTPRe 匹配 git 客户端的协议端点。
//
// 注意路径里带 /git/ 前缀：mux.Handle("/git/", h) 只做前缀匹配，
// **不会**像 StripPrefix 那样剥离前缀，所以 r.URL.Path 仍然是
// /git/<repo>.git/info/refs 的完整形式。
//
//	GET  /git/<repo>.git/info/refs?service=git-upload-pack
//	POST /git/<repo>.git/git-upload-pack
//	POST /git/<repo>.git/git-receive-pack
var smartHTTPRe = regexp.MustCompile(
	`^/git/([a-z0-9][a-z0-9._-]{0,63})\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

// ServeHTTP 实现 http.Handler，挂载在 /git/ 下。
type GitHTTPHandler struct {
	svc *GitService

	// enabled 返回仓库访问是否开启。关闭时一律 404，
	// 让公网完全看不到仓库存在——这是「用完就关」的核心。
	enabled func() bool

	// auth 校验请求凭据。返回 true 表示放行。
	//
	// 两种来源：
	//   1. HTTP Basic（Codemagic 用：用户名任意，密码填 access token）
	//   2. 面板会话 Cookie（浏览器里点链接直接看，不用再输密码）
	auth func(r *http.Request) bool
}

// NewGitHTTPHandler 创建协议处理器。
func NewGitHTTPHandler(svc *GitService, enabled func() bool, auth func(*http.Request) bool) *GitHTTPHandler {
	return &GitHTTPHandler{svc: svc, enabled: enabled, auth: auth}
}

func (h *GitHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 总开关。关闭时返回 404 而不是 403：
	// 403 等于告诉扫描器「这里有东西但不让你看」，
	// 404 什么都不泄露。
	if h.enabled == nil || !h.enabled() {
		http.NotFound(w, r)
		return
	}

	m := smartHTTPRe.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	repoName, endpoint := m[1], m[2]

	if !h.svc.Exists(repoName) {
		http.NotFound(w, r)
		return
	}

	// 鉴权。放在「仓库存在」之后，避免未授权者探测仓库名。
	if h.auth == nil || !h.auth(r) {
		// 返回 401 并带上 WWW-Authenticate，git 客户端才会弹认证
		// 或使用配置好的凭据重试。缺这个头，git 只会报「认证失败」而不重试。
		w.Header().Set("WWW-Authenticate", `Basic realm="Nova Git"`)
		http.Error(w, "需要认证", http.StatusUnauthorized)
		return
	}

	// 只读端点允许 GET，写入端点必须 POST。
	// 严格校验方法能挡掉一批无意义的探测请求。
	switch endpoint {
	case "info/refs":
		if r.Method != http.MethodGet {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
	case "git-upload-pack", "git-receive-pack":
		if r.Method != http.MethodPost {
			http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
			return
		}
	}

	h.runBackend(w, r, repoName, endpoint)
}

// runBackend 调用 git http-backend 并把结果写回 HTTP 响应。
func (h *GitHTTPHandler) runBackend(w http.ResponseWriter, r *http.Request, repoName, endpoint string) {
	// PATH_INFO 用 /<repo>.git/<endpoint> 的形式。
	// GIT_PROJECT_ROOT 指到仓库根目录，git 会拼成 <root>/<repo>.git。
	pathInfo := "/" + repoName + ".git/" + endpoint

	cmd := exec.Command("git", "http-backend")
	cmd.Env = append(os.Environ(),
		"GIT_PROJECT_ROOT="+h.svc.Root(),
		"GIT_HTTP_EXPORT_ALL=1", // 不依赖 git-daemon-export-ok 文件
		"PATH_INFO="+pathInfo,
		"REQUEST_METHOD="+r.Method,
		"QUERY_STRING="+r.URL.RawQuery,
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"REMOTE_USER=nova",
		"REMOTE_ADDR="+r.RemoteAddr,
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_PROTOCOL="+r.Proto,
		"SERVER_SOFTWARE=nova-panel",
		// 只读端点强制关闭写入能力。
		// 客户端即使往 upload-pack 发 push，也推不进去。
		"GIT_HTTP_MAX_REQUEST_BUFFER=100M",
	)

	// 请求体转给 CGI 的 stdin。
	//
	// 不能直接 cmd.Stdin = r.Body 就完事：push 的 packfile 可能有几十 MB，
	// 需要正确传递 Content-Length，否则 git 读不全。这里原样透传。
	if r.Body != nil {
		cmd.Stdin = r.Body
		if r.ContentLength > 0 {
			cmd.Env = append(cmd.Env, "CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10))
		}
	} else {
		cmd.Env = append(cmd.Env, "CONTENT_LENGTH=0")
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "服务内部错误", http.StatusInternalServerError)
		return
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		http.Error(w, "无法启动 git 后端", http.StatusInternalServerError)
		return
	}

	// CGI 输出格式：先是若干头部行（Status: / Content-Type: / ...），
	// 一个空行，然后才是响应体。必须先解析头部再写状态码，
	// 否则状态码已经发出去了就没法改。
	br := newCGIReader(stdout)
	status, headers, err := br.ReadHeaders()
	if err != nil {
		cmd.Wait()
		http.Error(w, "git 后端无响应", http.StatusInternalServerError)
		return
	}

	for k, v := range headers {
		w.Header().Set(k, v)
	}
	// 关键：git 的响应必须禁止缓存，否则中间层可能缓存 refs 列表，
	// 导致客户端拿到过期的分支信息。
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	w.Header().Set("Pragma", "no-cache")

	w.WriteHeader(status)

	// 流式拷贝响应体：clone 一个仓库可能返回几十 MB 的 packfile，
	// 读进内存再写会 OOM。
	io.Copy(w, br)

	if err := cmd.Wait(); err != nil {
		// 客户端提前断开（比如 Ctrl+C）也会走到这里，不必当成错误
		msg := strings.TrimSpace(stderr.String())
		if msg != "" && !strings.Contains(msg, "broken pipe") {
			// 已经写过响应头了，只能记日志
			fmt.Printf("git http-backend 退出异常: %v: %s\n", err, msg)
		}
	}
}

// ---------------------------------------------------------------- CGI 解析

// cgiReader 从 git http-backend 的输出里分离头部与响应体。
type cgiReader struct {
	r   io.Reader
	buf []byte
}

func newCGIReader(r io.Reader) *cgiReader {
	return &cgiReader{r: r}
}

// ReadHeaders 解析 CGI 头部，返回状态码与头部集合。
// 解析完的 reader 后续只输出响应体。
func (c *cgiReader) ReadHeaders() (int, map[string]string, error) {
	headers := make(map[string]string)
	status := http.StatusOK

	// 逐行读，直到空行。CGI 头部都很短，不需要担心性能。
	for {
		line, err := c.readLine()
		if err != nil {
			return 0, nil, err
		}
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if strings.EqualFold(key, "Status") {
			// 形如 "200 OK"
			if codeStr, _, ok := strings.Cut(value, " "); ok {
				if code, err := strconv.Atoi(codeStr); err == nil {
					status = code
				}
			} else if code, err := strconv.Atoi(value); err == nil {
				status = code
			}
			continue
		}
		headers[key] = value
	}

	return status, headers, nil
}

// readLine 读一行（不含 \n），并处理 \r\n。
func (c *cgiReader) readLine() (string, error) {
	var sb strings.Builder
	one := make([]byte, 1)
	for {
		n, err := c.r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return strings.TrimSuffix(sb.String(), "\r"), nil
			}
			sb.WriteByte(one[0])
		}
		if err != nil {
			if sb.Len() > 0 {
				return strings.TrimSuffix(sb.String(), "\r"), nil
			}
			return "", err
		}
	}
}

// Read 实现 io.Reader，让 cgiReader 能直接作为响应体源。
func (c *cgiReader) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

// EnsureRepoDirs 确保仓库根目录存在。
//
// 放在 service 里而不是 Create 里，是因为服务启动时就该准备好，
// 否则第一次建仓库才发现目录权限不对。
func (g *GitService) EnsureRepoDirs() error {
	if err := os.MkdirAll(g.root, 0o755); err != nil {
		return fmt.Errorf("创建仓库目录失败: %w", err)
	}
	return nil
}
