// Package api HTTP 接口。
//
// 约定：
//   - 所有响应都是 {"ok":bool,"data":...,"error":"..."}
//   - 除登录外所有接口都要会话
//   - 除 GET 外所有接口都要 CSRF token（放在 X-CSRF-Token 头）
//   - 所有写操作都记审计日志
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/sclt77/nova-mb/internal/auth"
	"github.com/sclt77/nova-mb/internal/config"
	"github.com/sclt77/nova-mb/internal/runner"
	"github.com/sclt77/nova-mb/internal/service"
	"github.com/sclt77/nova-mb/internal/store"
)

// Server HTTP 服务。
type Server struct {
	cfg    *config.Config
	store  *store.Store
	auth   *auth.Manager
	runner *runner.Runner

	sys    *service.SystemService
	logs   *service.LogService
	deploy *service.DeployService
	backup *service.BackupService
	domain *service.DomainService
	db     *service.DBService
	cron   *service.CronService
	git    *service.GitService

	mux *http.ServeMux
}

// New 创建服务。
func New(cfg *config.Config, st *store.Store, am *auth.Manager, r *runner.Runner) *Server {
	s := &Server{
		cfg:    cfg,
		store:  st,
		auth:   am,
		runner: r,
		sys:    service.NewSystemService(cfg),
		logs:   service.NewLogService(cfg),
		deploy: service.NewDeployService(cfg, r),
		backup: service.NewBackupService(cfg, r),
		domain: service.NewDomainService(cfg, r),
		db:     service.NewDBService(cfg, r),
		cron:   service.NewCronService(cfg, r),
		git:    service.NewGitService(cfg.GitRoot, cfg.GitTokenFile),
	}

	// 仓库根目录在启动时就建好，避免第一次建仓库才发现权限问题
	if err := s.git.EnsureRepoDirs(); err != nil {
		log.Printf("准备代码仓库目录失败: %v", err)
	}

	s.routes()
	return s
}

// ---------------------------------------------------------------- 路由

func (s *Server) routes() {
	m := http.NewServeMux()

	// 认证（无需登录）
	m.HandleFunc("POST /api/login", s.handleLogin)
	m.HandleFunc("POST /api/logout", s.handleLogout)
	m.HandleFunc("GET /api/session", s.handleSession)

	// 概览
	m.HandleFunc("GET /api/overview", s.authed(s.handleOverview))

	// 服务控制
	m.HandleFunc("POST /api/service/{action}", s.authed(s.handleServiceAction))

	// 部署
	m.HandleFunc("GET /api/deploy/releases", s.authed(s.handleReleases))
	m.HandleFunc("POST /api/deploy/upload", s.authed(s.handleUpload))
	m.HandleFunc("POST /api/deploy/run", s.authed(s.handleDeployRun))
	m.HandleFunc("POST /api/deploy/rollback", s.authed(s.handleRollback))

	// 备份
	m.HandleFunc("GET /api/backups", s.authed(s.handleBackups))
	m.HandleFunc("POST /api/backups/run", s.authed(s.handleBackupRun))
	m.HandleFunc("POST /api/backups/verify", s.authed(s.handleBackupVerify))
	m.HandleFunc("POST /api/backups/restore", s.authed(s.handleRestore))
	m.HandleFunc("DELETE /api/backups/{stamp}", s.authed(s.handleDeleteBackup))

	// 域名与证书
	m.HandleFunc("GET /api/domain", s.authed(s.handleDomainGet))
	m.HandleFunc("POST /api/domain", s.authed(s.handleDomainBind))
	m.HandleFunc("POST /api/domain/cert", s.authed(s.handleIssueCert))
	m.HandleFunc("POST /api/domain/cert/renew", s.authed(s.handleRenewCert))

	// 数据库
	m.HandleFunc("GET /api/database", s.authed(s.handleDatabase))
	m.HandleFunc("GET /api/database/slow", s.authed(s.handleSlowQueries))

	// 定时任务
	m.HandleFunc("GET /api/cron", s.authed(s.handleCronList))
	m.HandleFunc("POST /api/cron/{action}", s.authed(s.handleCronAction))

	// 日志
	m.HandleFunc("GET /api/logs", s.authed(s.handleLogFiles))
	m.HandleFunc("GET /api/logs/{key}", s.authed(s.handleLogTail))

	// 审计
	m.HandleFunc("GET /api/audit", s.authed(s.handleAudit))

	// 改密码
	m.HandleFunc("POST /api/password", s.authed(s.handleChangePassword))

	// 代码仓库（面板管理用）
	m.HandleFunc("GET /api/git", s.authed(s.handleGitOverview))
	m.HandleFunc("POST /api/git/repos", s.authed(s.handleGitCreate))
	m.HandleFunc("DELETE /api/git/repos/{name}", s.authed(s.handleGitDelete))
	m.HandleFunc("GET /api/git/repos/{name}/commits", s.authed(s.handleGitCommits))
	m.HandleFunc("GET /api/git/repos/{name}/tree", s.authed(s.handleGitTree))
	m.HandleFunc("GET /api/git/repos/{name}/blob", s.authed(s.handleGitBlob))
	m.HandleFunc("POST /api/git/toggle", s.authed(s.handleGitToggle))
	m.HandleFunc("POST /api/git/token/reset", s.authed(s.handleGitResetToken))

	s.mux = m
}

// Handler 返回带中间件的根 handler。
func (s *Server) Handler(static http.Handler) http.Handler {
	mux := http.NewServeMux()

	// API 走自己的路由
	mux.Handle("/api/", s.mux)

	// Git 协议端点。
	//
	// 单独挂在 /git/ 而不是 /api/ 下：git 客户端请求的是
	// /<repo>.git/info/refs 这类固定格式的路径，不能带额外前缀。
	// 鉴权走 Basic（token），不是面板会话——CI 系统没有 Cookie。
	mux.Handle("/git/", service.NewGitHTTPHandler(
		s.git,
		s.gitEnabled,
		s.gitRequestAuth,
	))

	// 其余交给静态文件
	mux.Handle("/", static)

	return securityHeaders(mux)
}

// gitRequestAuth 校验 git 协议请求的凭据。
//
// 支持两种：
//  1. HTTP Basic —— Codemagic / git CLI 用。用户名随意（统一按 nova 处理），
//     密码必须是 access token。
//  2. 面板会话 Cookie —— 在浏览器里直接点 clone 链接时用，省得再输密码。
//
// 注意：不能用面板的账号密码做 Basic 认证。那等于把后台管理权限
// 交给 CI 系统，而且用户改面板密码会静默打断构建。
func (s *Server) gitRequestAuth(r *http.Request) bool {
	// 先试 Basic
	if _, pass, ok := r.BasicAuth(); ok {
		return s.git.CheckToken(pass)
	}

	// 再试面板会话（浏览器场景）
	if sess, _, err := s.auth.SessionFromRequest(r.Context(), r); err == nil && sess != nil {
		return true
	}

	return false
}

// securityHeaders 加安全响应头。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// 禁止被嵌入 iframe，防点击劫持
		h.Set("X-Frame-Options", "DENY")
		// 禁止 MIME 嗅探
		h.Set("X-Content-Type-Options", "nosniff")
		// 不泄露来源路径
		h.Set("Referrer-Policy", "no-referrer")
		// 面板不需要摄像头麦克风定位
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")

		// CSP：只允许同源脚本与样式。
		// 前端是原生 JS，没有内联脚本，所以可以收紧到 'self'。
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")

		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- 中间件

type ctxKey int

const (
	ctxSession ctxKey = iota
	ctxAdmin
)

// authed 包装需要登录的 handler，并校验 CSRF。
func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()

		sess, admin, err := s.auth.SessionFromRequest(ctx, r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}

		// 非 GET 请求校验 CSRF
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			token := r.Header.Get("X-CSRF-Token")
			if !auth.CheckCSRF(sess, token) {
				s.audit(ctx, admin, "csrf_failed", r.URL.Path, auth.ClientIP(r), false)
				writeErr(w, http.StatusForbidden, "CSRF 校验失败，请刷新页面重试")
				return
			}
		}

		ctx = context.WithValue(ctx, ctxSession, sess)
		ctx = context.WithValue(ctx, ctxAdmin, admin)

		h(w, r.WithContext(ctx))
	}
}

func sessionFrom(ctx context.Context) *store.Session {
	s, _ := ctx.Value(ctxSession).(*store.Session)
	return s
}

func adminFrom(ctx context.Context) *store.Admin {
	a, _ := ctx.Value(ctxAdmin).(*store.Admin)
	return a
}

// audit 写审计日志。
func (s *Server) audit(ctx context.Context, admin *store.Admin, action, detail, ip string, ok bool) {
	if admin == nil {
		return
	}
	if err := s.store.Log(ctx, admin.ID, admin.Username, action, detail, ip, ok); err != nil {
		log.Printf("写审计日志失败: %v", err)
	}
}

// ---------------------------------------------------------------- 响应

type response struct {
	OK    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

func writeOK(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(response{OK: true, Data: data})
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response{OK: false, Error: msg})
}

// writeResult 把命令执行结果转成响应。
func writeResult(w http.ResponseWriter, res *runner.Result) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(response{OK: res.OK, Data: res})
}

func readJSON(r *http.Request, dst any) error {
	// 限制请求体大小，防内存耗尽
	limited := io.LimitReader(r.Body, 1<<20)
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// ---------------------------------------------------------------- 认证接口

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var req loginReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, "请输入用户名和密码")
		return
	}

	ip := auth.ClientIP(r)
	ua := r.UserAgent()

	sess, err := s.auth.Login(ctx, req.Username, req.Password, ip, ua)
	if err != nil {
		// 登录失败也记日志，便于发现暴力破解
		_ = s.store.Log(ctx, 0, req.Username, "login_failed", "", ip, false)
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}

	s.auth.SetCookie(w, sess)

	admin, _ := s.store.AdminByID(ctx, sess.AdminID)
	if admin != nil {
		s.audit(ctx, admin, "login", "", ip, true)
	}

	writeOK(w, map[string]any{
		"username": req.Username,
		"csrfToken": sess.CSRFToken,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	sess, admin, err := s.auth.SessionFromRequest(ctx, r)
	if err == nil {
		if admin != nil {
			s.audit(ctx, admin, "logout", "", auth.ClientIP(r), true)
		}
		_ = s.auth.Logout(ctx, sess.Token)
	}

	s.auth.ClearCookie(w)
	writeOK(w, nil)
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	sess, admin, err := s.auth.SessionFromRequest(ctx, r)
	if err != nil {
		writeOK(w, map[string]any{"authenticated": false})
		return
	}

	writeOK(w, map[string]any{
		"authenticated": true,
		"username":      admin.Username,
		"csrfToken":     sess.CSRFToken,
		"expiresAt":     sess.ExpiresAt,
	})
}

type passwordReq struct {
	Current string `json:"current"`
	New     string `json:"new"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)
	sess := sessionFrom(ctx)

	var req passwordReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	if !auth.CheckPassword(admin.PasswordHash, req.Current) {
		writeErr(w, http.StatusBadRequest, "当前密码不正确")
		return
	}

	hash, err := auth.HashPassword(req.New)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.store.UpdatePassword(ctx, admin.ID, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, "修改失败")
		return
	}

	// 改完密码让其他设备下线，当前会话保留
	_ = s.store.DeleteOtherSessions(ctx, admin.ID, sess.Token)

	s.audit(ctx, admin, "change_password", "", auth.ClientIP(r), true)
	writeOK(w, map[string]any{"message": "密码已修改，其他设备已下线"})
}

// ---------------------------------------------------------------- 概览

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ov := s.sys.CollectOverview()
	writeOK(w, ov)
}

// ---------------------------------------------------------------- 服务控制

func (s *Server) handleServiceAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	action := r.PathValue("action")

	var body struct {
		Service string `json:"service"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	var actionName string
	switch action {
	case "start":
		actionName = "service.start"
	case "stop":
		actionName = "service.stop"
	case "restart":
		actionName = "service.restart"
	default:
		writeErr(w, http.StatusBadRequest, "不支持的操作")
		return
	}

	res, err := s.runner.Run(actionName, map[string]string{"service": body.Service})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "service."+action, body.Service, auth.ClientIP(r), res.OK)
	writeResult(w, res)
}

// ---------------------------------------------------------------- 部署

func (s *Server) handleReleases(w http.ResponseWriter, r *http.Request) {
	releases, err := s.deploy.Releases()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取版本列表失败")
		return
	}
	writeOK(w, releases)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	// 限制整体请求大小
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+1<<20)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "文件过大或格式错误")
		return
	}

	file, header, err := r.FormFile("package")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "请选择要上传的部署包")
		return
	}
	defer file.Close()

	res, err := s.deploy.UploadPackage(header.Filename, file)
	if err != nil {
		s.audit(ctx, admin, "deploy.upload", header.Filename, auth.ClientIP(r), false)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "deploy.upload",
		fmt.Sprintf("%s (%d MB)", header.Filename, res.Size/1024/1024),
		auth.ClientIP(r), true)

	writeOK(w, res)
}

func (s *Server) handleDeployRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	res, err := s.deploy.Deploy(body.Path)
	if err != nil {
		s.audit(ctx, admin, "deploy.run", body.Path, auth.ClientIP(r), false)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "deploy.run", body.Path, auth.ClientIP(r), res.OK)

	// 部署完成后清理旧包，只留最近 3 个
	if res.OK {
		s.deploy.CleanPackages(3)
	}

	writeResult(w, res)
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	res, err := s.deploy.Rollback()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "deploy.rollback", "", auth.ClientIP(r), res.OK)
	writeResult(w, res)
}

// ---------------------------------------------------------------- 备份

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	list, err := s.backup.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取备份列表失败")
		return
	}

	writeOK(w, map[string]any{
		"items":     list,
		"totalSize": s.backup.TotalSize(),
	})
}

func (s *Server) handleBackupRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	var body struct {
		Full bool `json:"full"`
	}
	_ = readJSON(r, &body)

	res, err := s.backup.RunBackup(body.Full)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "backup.run", map[bool]string{true: "全量", false: "仅数据库"}[body.Full],
		auth.ClientIP(r), res.OK)
	writeResult(w, res)
}

func (s *Server) handleBackupVerify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	res, err := s.backup.Verify()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "backup.verify", "", auth.ClientIP(r), res.OK)
	writeResult(w, res)
}

func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	var body struct {
		Stamp string `json:"stamp"`
		Scope string `json:"scope"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	res, err := s.backup.Restore(body.Stamp, body.Scope)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "backup.restore",
		fmt.Sprintf("%s (%s)", body.Stamp, body.Scope), auth.ClientIP(r), res.OK)
	writeResult(w, res)
}

func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	stamp := r.PathValue("stamp")
	if err := s.backup.DeleteBackup(stamp); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "backup.delete", stamp, auth.ClientIP(r), true)
	writeOK(w, nil)
}

// ---------------------------------------------------------------- 域名

func (s *Server) handleDomainGet(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.domain.Get())
}

func (s *Server) handleDomainBind(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	var body struct {
		Domain string `json:"domain"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	if err := s.domain.Bind(body.Domain); err != nil {
		s.audit(ctx, admin, "domain.bind", body.Domain, auth.ClientIP(r), false)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "domain.bind", body.Domain, auth.ClientIP(r), true)
	writeOK(w, map[string]any{"domain": body.Domain, "message": "域名已绑定"})
}

func (s *Server) handleIssueCert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	var body struct {
		Domain string `json:"domain"`
		Email  string `json:"email"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	res, err := s.domain.IssueCert(body.Domain, body.Email)
	if err != nil {
		s.audit(ctx, admin, "cert.issue", body.Domain, auth.ClientIP(r), false)
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "cert.issue", body.Domain, auth.ClientIP(r), res.OK)
	writeResult(w, res)
}

func (s *Server) handleRenewCert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	res, err := s.domain.RenewCert()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "cert.renew", "", auth.ClientIP(r), res.OK)
	writeResult(w, res)
}

// ---------------------------------------------------------------- 数据库

func (s *Server) handleDatabase(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.db.Info())
}

func (s *Server) handleSlowQueries(w http.ResponseWriter, r *http.Request) {
	info, err := s.db.SlowQueries()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取失败")
		return
	}
	writeOK(w, info)
}

// ---------------------------------------------------------------- 定时任务

func (s *Server) handleCronList(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.cron.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取定时任务失败")
		return
	}
	writeOK(w, tasks)
}

func (s *Server) handleCronAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	admin := adminFrom(ctx)

	action := r.PathValue("action")

	var res *runner.Result
	var err error

	switch action {
	case "install":
		res, err = s.cron.Install()
	case "remove":
		res, err = s.cron.Remove()
	default:
		writeErr(w, http.StatusBadRequest, "不支持的操作")
		return
	}

	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.audit(ctx, admin, "cron."+action, "", auth.ClientIP(r), res.OK)
	writeResult(w, res)
}

// ---------------------------------------------------------------- 日志

func (s *Server) handleLogFiles(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.logs.Files())
}

func (s *Server) handleLogTail(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")

	if !runner.ValidLogName(key) {
		writeErr(w, http.StatusBadRequest, "不支持的日志类型")
		return
	}

	lines := 500
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &lines); n == 1 && err != nil {
			lines = 500
		}
	}

	content, err := s.logs.Tail(key, lines)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeOK(w, map[string]any{"key": key, "content": content})
}

// ---------------------------------------------------------------- 审计

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		fmt.Sscanf(v, "%d", &page)
	}
	if page < 1 {
		page = 1
	}

	const size = 50
	logs, total, err := s.store.ListLogs(r.Context(), size, (page-1)*size)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取审计日志失败")
		return
	}

	writeOK(w, map[string]any{
		"items": logs,
		"total": total,
		"page":  page,
		"size":  size,
	})
}

// ---------------------------------------------------------------- 启动

// Run 启动 HTTP 服务。
func (s *Server) Run(static http.Handler) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr(),
		Handler:           s.Handler(static),
		ReadHeaderTimeout: 10 * time.Second,

		// ReadTimeout 必须放宽到分钟级，不能是几十秒。
		//
		// 它限制的是「整个请求体读完」的耗时，从连接建立起就计时。
		// 部署包有 70MB+，按 30 秒算，网速低于 2.4MB/s 就传不完，
		// Go 会直接掐断连接 —— 浏览器侧只看到 `failed to fetch`，
		// 没有 HTTP 状态码、没有响应体，面板日志里也不会有任何记录
		// （请求在读取阶段就被丢弃了，还没进到 handler）。
		//
		// 这个坑极难排查：上传目录是空的、审计日志没有记录、
		// nginx 错误日志也没有 413，看起来像「什么都没发生」。
		//
		// 30 分钟足够传 1GB（约 570KB/s），对家宽上行也够用。
		ReadTimeout: 30 * time.Minute,

		// 部署、备份、恢复都是长任务，写超时同样要放宽
		WriteTimeout: 35 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	log.Printf("面板监听 %s（TLS: %v）", s.cfg.Addr(), s.cfg.UseTLS)

	if s.cfg.UseTLS {
		return srv.ListenAndServeTLS(s.cfg.CertFile, s.cfg.KeyFile)
	}
	return srv.ListenAndServe()
}

// 确保 errors 被使用（保留以便后续扩展）
var _ = errors.Is
