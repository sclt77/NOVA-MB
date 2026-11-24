// Package auth 面板认证：密码哈希、会话 Cookie、CSRF、登录限流。
//
// 独立于 Nova 业务账号体系。面板能启停服务、部署代码、恢复数据库，
// 复用一个面向普通用户的账号表是不合适的——权限模型完全不同。
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/sclt77/nova-mb/internal/config"
	"github.com/sclt77/nova-mb/internal/store"
)

const (
	// Cookie 名
	sessionCookie = "nova_panel_session"

	// 会话有效期
	sessionTTL = 8 * time.Hour

	// bcrypt 成本。12 在现代硬件上约 250ms，够安全又不至于让登录变慢。
	bcryptCost = 12
)

// Manager 认证管理器。
type Manager struct {
	store  *store.Store
	secure bool // 是否设置 Secure Cookie（HTTPS 时必须）

	// 登录限流：同一 IP 的失败次数
	mu       sync.Mutex
	attempts map[string]*attemptInfo
}

type attemptInfo struct {
	count    int
	firstAt  time.Time
	lockedTo time.Time
}

// New 创建认证管理器。
func New(st *store.Store, secure bool) *Manager {
	m := &Manager{
		store:    st,
		secure:   secure,
		attempts: make(map[string]*attemptInfo),
	}
	go m.cleanupLoop()
	return m
}

// HashPassword 生成密码哈希。
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", errors.New("密码至少 8 位")
	}
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("生成密码哈希失败: %w", err)
	}
	return string(b), nil
}

// CheckPassword 校验密码。
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// ErrLocked 登录被锁定。
var ErrLocked = errors.New("登录失败次数过多，请 15 分钟后再试")

// ErrBadCredentials 账号或密码错误。
var ErrBadCredentials = errors.New("用户名或密码错误")

// Login 登录，成功后返回会话 token 与 CSRF token。
func (m *Manager) Login(ctx context.Context, username, password, ip, ua string) (*store.Session, error) {
	// IP 维度限流，先于数据库查询：防止用不存在的用户名反复试探
	if err := m.checkIPLocked(ip); err != nil {
		return nil, err
	}

	admin, err := m.store.AdminByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		// 不区分「用户不存在」和「密码错误」，避免账号枚举。
		// 但仍然算一次失败，防止有人拿这个接口探测账号是否存在。
		m.recordIPFailure(ip)
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err
	}

	// 账号维度锁定
	if admin.LockedUntil != nil && time.Now().Before(*admin.LockedUntil) {
		remain := time.Until(*admin.LockedUntil).Minutes()
		return nil, fmt.Errorf("账号已锁定，请 %.0f 分钟后再试", remain+1)
	}

	if !CheckPassword(admin.PasswordHash, password) {
		count, _ := m.store.RecordLoginFailure(ctx, admin.ID)
		m.recordIPFailure(ip)
		if count >= 5 {
			return nil, ErrLocked
		}
		return nil, ErrBadCredentials
	}

	// 成功：清失败计数
	_ = m.store.RecordLoginSuccess(ctx, admin.ID, ip)
	m.clearIPFailure(ip)

	// 建会话
	token, err := config.RandomHex(32)
	if err != nil {
		return nil, err
	}
	csrf, err := config.RandomHex(24)
	if err != nil {
		return nil, err
	}

	sess := &store.Session{
		Token:     token,
		AdminID:   admin.ID,
		CSRFToken: csrf,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(sessionTTL),
		IP:        ip,
		UserAgent: truncate(ua, 200),
	}

	if err := m.store.CreateSession(ctx, sess); err != nil {
		return nil, err
	}

	return sess, nil
}

// SetCookie 写会话 Cookie。
func (m *Manager) SetCookie(w http.ResponseWriter, sess *store.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    sess.Token,
		Path:     "/",
		HttpOnly: true,           // 禁止 JS 读取，防 XSS 偷 token
		Secure:   m.secure,       // HTTPS 时只走加密通道
		SameSite: http.SameSiteStrictMode, // 防 CSRF
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

// ClearCookie 清除会话 Cookie。
func (m *Manager) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// SessionFromRequest 从请求里取会话。
func (m *Manager) SessionFromRequest(ctx context.Context, r *http.Request) (*store.Session, *store.Admin, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, nil, store.ErrNotFound
	}

	sess, err := m.store.SessionByToken(ctx, c.Value)
	if err != nil {
		return nil, nil, err
	}

	admin, err := m.store.AdminByID(ctx, sess.AdminID)
	if err != nil {
		return nil, nil, err
	}

	return sess, admin, nil
}

// Logout 登出。
func (m *Manager) Logout(ctx context.Context, token string) error {
	return m.store.DeleteSession(ctx, token)
}

// CheckCSRF 校验 CSRF token。
//
// 用常量时间比较，避免通过响应时间差推断 token。
func CheckCSRF(sess *store.Session, provided string) bool {
	if sess == nil || provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(sess.CSRFToken), []byte(provided)) == 1
}

// ---------------------------------------------------------------- IP 限流

// checkIPLocked 检查 IP 是否被锁。
func (m *Manager) checkIPLocked(ip string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	info, ok := m.attempts[ip]
	if !ok {
		return nil
	}

	if time.Now().Before(info.lockedTo) {
		remain := time.Until(info.lockedTo).Minutes()
		return fmt.Errorf("尝试过于频繁，请 %.0f 分钟后再试", remain+1)
	}

	return nil
}

// recordIPFailure 记录 IP 失败。
//
// 10 分钟内失败 10 次就锁 15 分钟。比账号锁定更宽松一些，
// 避免同一出口 IP（比如公司网络）的其他人被误伤。
func (m *Manager) recordIPFailure(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	info, ok := m.attempts[ip]
	if !ok || now.Sub(info.firstAt) > 10*time.Minute {
		m.attempts[ip] = &attemptInfo{count: 1, firstAt: now}
		return
	}

	info.count++
	if info.count >= 10 {
		info.lockedTo = now.Add(15 * time.Minute)
	}
}

func (m *Manager) clearIPFailure(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.attempts, ip)
}

// cleanupLoop 定期清理过期的限流记录与过期会话。
func (m *Manager) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		m.mu.Lock()
		now := time.Now()
		for ip, info := range m.attempts {
			if now.Sub(info.firstAt) > 30*time.Minute && now.After(info.lockedTo) {
				delete(m.attempts, ip)
			}
		}
		m.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, _ = m.store.PurgeExpiredSessions(ctx)
		cancel()
	}
}

// ---------------------------------------------------------------- 工具

// ClientIP 取客户端真实 IP。
//
// 面板默认只监听 127.0.0.1，正常走 SSH 端口转发时是 127.0.0.1。
// 如果前面有 Nginx，X-Real-IP 才是真实来源。
func ClientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
