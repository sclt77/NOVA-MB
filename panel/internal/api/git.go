package api

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/sclt77/nova-mb/internal/auth"
)

// ---------------------------------------------------------------- 代码仓库

// 这一组接口都是「面板登录后」用的：建仓库、看历史、开关访问、取令牌。
// 真正的 git 协议由 GitHTTPHandler 在 /git/ 下单独处理，
// 走的是 Basic 认证而不是面板会话——因为 git 客户端不支持 Cookie 流程。

// handleGitOverview 仓库列表 + 访问状态。
func (s *Server) handleGitOverview(w http.ResponseWriter, r *http.Request) {
	baseURL := publicBaseURL(r)

	repos, err := s.git.List(baseURL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取仓库列表失败："+err.Error())
		return
	}

	enabled := s.gitEnabled()
	token := s.git.Token()

	writeOK(w, map[string]any{
		"enabled":  enabled,
		"repos":    repos,
		"token":    token,
		"baseUrl":  baseURL,
		"gitPath":  "/git/",
		"username": "nova",
		// 关掉时也把 token 带出去：用户可能需要先复制再去开启，
		// token 本身不算敏感（没有开启状态它用不了）。
		"hint": "Codemagic 里仓库地址填 cloneUrl，用户名填 nova，密码填 access token",
	})
}

// handleGitCreate 创建仓库。
func (s *Server) handleGitCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	name := strings.TrimSpace(strings.ToLower(body.Name))
	// 允许用户直接粘贴 novaqd.git，自动去掉后缀
	name = strings.TrimSuffix(name, ".git")

	repo, err := s.git.Create(name, strings.TrimSpace(body.Description), publicBaseURL(r))
	if err != nil {
		s.audit(ctx, admin, "git.create", name, auth.ClientIP(r), false)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "git.create", name, auth.ClientIP(r), true)
	writeOK(w, repo)
}

// handleGitDelete 删除仓库。
func (s *Server) handleGitDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)
	name := r.PathValue("name")

	if err := s.git.Delete(name); err != nil {
		s.audit(ctx, admin, "git.delete", name, auth.ClientIP(r), false)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "git.delete", name, auth.ClientIP(r), true)
	writeOK(w, nil)
}

// handleGitCommits 提交历史。
func (s *Server) handleGitCommits(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	commits, err := s.git.Commits(name, 100)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, commits)
}

// handleGitTree 文件树。
func (s *Server) handleGitTree(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ref := r.URL.Query().Get("ref")
	path := r.URL.Query().Get("path")

	entries, err := s.git.Tree(name, ref, path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, entries)
}

// handleGitBlob 文件内容。
func (s *Server) handleGitBlob(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ref := r.URL.Query().Get("ref")
	path := r.URL.Query().Get("path")

	content, err := s.git.Blob(name, ref, path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeOK(w, map[string]any{"content": content, "path": path})
}

// handleGitToggle 开关仓库公网访问。
//
// 这是整个功能的安全核心：平时关闭，公网访问 /git/ 一律 404；
// 要用 Codemagic 打包时打开，打完就关。
func (s *Server) handleGitToggle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	// 状态存 SQLite settings 表，重启后保持。
	// 不存内存变量：面板重启（或崩溃自动拉起）后应该维持用户设定的状态，
	// 而不是悄悄变回「关闭」——那样 CI 会突然拉不到代码且没人知道原因。
	value := "0"
	if body.Enabled {
		value = "1"
		// 开启前确保有 token，否则 Codemagic 无法认证
		if _, err := s.git.EnsureToken(); err != nil {
			writeErr(w, http.StatusInternalServerError, "生成访问令牌失败："+err.Error())
			return
		}
	}

	if err := s.store.SetSetting(ctx, "git_enabled", value); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存设置失败："+err.Error())
		return
	}

	action := "git.disable"
	if body.Enabled {
		action = "git.enable"
	}
	s.audit(ctx, admin, action, "", auth.ClientIP(r), true)

	writeOK(w, map[string]any{"enabled": body.Enabled})
}

// handleGitResetToken 重置访问令牌。
//
// 令牌泄露时用。重置后旧令牌立即失效，需要在 Codemagic 里更新。
func (s *Server) handleGitResetToken(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	// 删掉旧文件，再确保生成新的
	if err := os.Remove(s.cfg.GitTokenFile); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "删除旧令牌失败："+err.Error())
		return
	}

	token, err := s.git.EnsureToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成新令牌失败："+err.Error())
		return
	}

	s.audit(ctx, admin, "git.reset_token", "", auth.ClientIP(r), true)
	writeOK(w, map[string]any{"token": token})
}

// gitEnabled 读访问开关。
//
// 这里传 context.Background() 而不是 nil：database/sql 的
// QueryRowContext 会对 nil context 做非空断言，直接 panic。
func (s *Server) gitEnabled() bool {
	v, err := s.store.GetSetting(context.Background(), "git_enabled")
	return err == nil && v == "1"
}

// publicBaseURL 推断外部可访问的站点根地址。
//
// 优先级：NOVA_PUBLIC_URL 环境变量 > 请求头 > 请求 Host。
// 面板本身监听 127.0.0.1，用户是通过 SSH 端口转发访问的，
// 所以 r.Host 是 127.0.0.1:8888 —— 这个地址给 Codemagic 用毫无意义。
// 必须让用户能配一个真实地址（域名或公网 IP）。
func publicBaseURL(r *http.Request) string {
	if v := os.Getenv("NOVA_PUBLIC_URL"); v != "" {
		return strings.TrimSuffix(v, "/")
	}

	// 反向代理场景：优先用 X-Forwarded-*
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	} else if r.TLS != nil {
		scheme = "https"
	}

	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}

	// 127.0.0.1 / localhost 对 CI 不可用，提示用户去配 NOVA_PUBLIC_URL
	if strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "localhost") {
		return ""
	}
	return scheme + "://" + host
}
