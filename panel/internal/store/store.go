// Package store 面板自己的持久化：管理员账号、会话、审计日志。
//
// 用 SQLite（modernc.org/sqlite，纯 Go 实现，不需要 cgo）而不是 JSON 文件：
// 审计日志会持续增长，文件读写容易在并发下损坏。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store 面板数据存储。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）面板数据库。
func Open(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "panel.db")

	// _pragma 参数保证并发安全与崩溃恢复
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// SQLite 单写入者，限制连接数避免锁竞争
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}

	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS admins (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL,
    created_at    TEXT    NOT NULL,
    last_login_at TEXT,
    last_login_ip TEXT,
    failed_count  INTEGER NOT NULL DEFAULT 0,
    locked_until  TEXT
);

CREATE TABLE IF NOT EXISTS sessions (
    token       TEXT PRIMARY KEY,
    admin_id    INTEGER NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    csrf_token  TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    ip          TEXT NOT NULL DEFAULT '',
    user_agent  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS audit_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    admin_id   INTEGER NOT NULL,
    admin_name TEXT    NOT NULL,
    action     TEXT    NOT NULL,
    detail     TEXT    NOT NULL DEFAULT '',
    ip         TEXT    NOT NULL DEFAULT '',
    ok         INTEGER NOT NULL DEFAULT 1,
    created_at TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at DESC);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("建表失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- 管理员

// Admin 管理员账号。
type Admin struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	LastLoginAt  *time.Time
	LastLoginIP  string
	FailedCount  int
	LockedUntil  *time.Time
}

// ErrNotFound 记录不存在。
var ErrNotFound = errors.New("记录不存在")

// CountAdmins 管理员数量，用于判断是否需要初始化。
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM admins`).Scan(&n)
	return n, err
}

// CreateAdmin 创建管理员。
func (s *Store) CreateAdmin(ctx context.Context, username, hash string) (*Admin, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO admins (username, password_hash, created_at) VALUES (?, ?, ?)`,
		username, hash, now)
	if err != nil {
		return nil, fmt.Errorf("创建管理员失败: %w", err)
	}

	id, _ := res.LastInsertId()
	t, _ := time.Parse(time.RFC3339, now)
	return &Admin{ID: id, Username: username, PasswordHash: hash, CreatedAt: t}, nil
}

// AdminByUsername 按用户名查管理员。
func (s *Store) AdminByUsername(ctx context.Context, username string) (*Admin, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at, last_login_at,
		        COALESCE(last_login_ip,''), failed_count, locked_until
		 FROM admins WHERE username = ?`, username)

	return scanAdmin(row)
}

// AdminByID 按 id 查管理员。
func (s *Store) AdminByID(ctx context.Context, id int64) (*Admin, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at, last_login_at,
		        COALESCE(last_login_ip,''), failed_count, locked_until
		 FROM admins WHERE id = ?`, id)

	return scanAdmin(row)
}

func scanAdmin(row *sql.Row) (*Admin, error) {
	var a Admin
	var created string
	var lastLogin, locked sql.NullString

	err := row.Scan(&a.ID, &a.Username, &a.PasswordHash, &created,
		&lastLogin, &a.LastLoginIP, &a.FailedCount, &locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	a.CreatedAt, _ = time.Parse(time.RFC3339, created)
	if lastLogin.Valid {
		if t, err := time.Parse(time.RFC3339, lastLogin.String); err == nil {
			a.LastLoginAt = &t
		}
	}
	if locked.Valid {
		if t, err := time.Parse(time.RFC3339, locked.String); err == nil {
			a.LockedUntil = &t
		}
	}

	return &a, nil
}

// RecordLoginSuccess 记录登录成功，清空失败计数。
func (s *Store) RecordLoginSuccess(ctx context.Context, id int64, ip string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE admins SET last_login_at = ?, last_login_ip = ?,
		        failed_count = 0, locked_until = NULL WHERE id = ?`,
		now, ip, id)
	return err
}

// RecordLoginFailure 记录登录失败，达到阈值则锁定。
//
// 锁定策略：连续失败 5 次锁 15 分钟。这是最基础的暴力破解防护，
// 没有它的话弱密码在公网上几分钟就会被撞开。
func (s *Store) RecordLoginFailure(ctx context.Context, id int64) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT failed_count FROM admins WHERE id = ?`, id).Scan(&count); err != nil {
		return 0, err
	}

	count++

	var lockedUntil any
	if count >= 5 {
		lockedUntil = time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339)
	}

	_, err := s.db.ExecContext(ctx,
		`UPDATE admins SET failed_count = ?, locked_until = ? WHERE id = ?`,
		count, lockedUntil, id)

	return count, err
}

// UpdatePassword 修改密码。
func (s *Store) UpdatePassword(ctx context.Context, id int64, hash string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE admins SET password_hash = ? WHERE id = ?`, hash, id)
	return err
}

// ---------------------------------------------------------------- 会话

// Session 登录会话。
type Session struct {
	Token     string
	AdminID   int64
	CSRFToken string
	CreatedAt time.Time
	ExpiresAt time.Time
	IP        string
	UserAgent string
}

// CreateSession 创建会话。
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token, admin_id, csrf_token, created_at, expires_at, ip, user_agent)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.Token, sess.AdminID, sess.CSRFToken,
		sess.CreatedAt.UTC().Format(time.RFC3339),
		sess.ExpiresAt.UTC().Format(time.RFC3339),
		sess.IP, sess.UserAgent)
	return err
}

// SessionByToken 按 token 查会话。过期的返回 ErrNotFound。
func (s *Store) SessionByToken(ctx context.Context, token string) (*Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT token, admin_id, csrf_token, created_at, expires_at, ip, user_agent
		 FROM sessions WHERE token = ?`, token)

	var sess Session
	var created, expires string

	err := row.Scan(&sess.Token, &sess.AdminID, &sess.CSRFToken,
		&created, &expires, &sess.IP, &sess.UserAgent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	sess.CreatedAt, _ = time.Parse(time.RFC3339, created)
	sess.ExpiresAt, _ = time.Parse(time.RFC3339, expires)

	if time.Now().After(sess.ExpiresAt) {
		_ = s.DeleteSession(ctx, token)
		return nil, ErrNotFound
	}

	return &sess, nil
}

// DeleteSession 删除会话（登出）。
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// DeleteOtherSessions 删除该管理员的其它会话（改密码后强制其他设备下线）。
func (s *Store) DeleteOtherSessions(ctx context.Context, adminID int64, keep string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE admin_id = ? AND token <> ?`, adminID, keep)
	return err
}

// PurgeExpiredSessions 清理过期会话，定时调用。
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------- 审计

// AuditLog 审计日志。
type AuditLog struct {
	ID        int64     `json:"id"`
	AdminName string    `json:"adminName"`
	Action    string    `json:"action"`
	Detail    string    `json:"detail"`
	IP        string    `json:"ip"`
	OK        bool      `json:"ok"`
	CreatedAt time.Time `json:"createdAt"`
}

// Log 写审计日志。
//
// 所有写操作都要记：谁、什么时候、从哪个 IP、做了什么、成功没有。
// 面板能启停服务、部署代码、恢复数据库，出了问题必须能追溯。
func (s *Store) Log(ctx context.Context, adminID int64, adminName, action, detail, ip string, ok bool) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_logs (admin_id, admin_name, action, detail, ip, ok, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		adminID, adminName, action, detail, ip, ok,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// ListLogs 分页查审计日志。
func (s *Store) ListLogs(ctx context.Context, limit, offset int) ([]AuditLog, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, admin_name, action, detail, ip, ok, created_at
		 FROM audit_logs ORDER BY id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []AuditLog
	for rows.Next() {
		var l AuditLog
		var created string
		if err := rows.Scan(&l.ID, &l.AdminName, &l.Action, &l.Detail,
			&l.IP, &l.OK, &created); err != nil {
			return nil, 0, err
		}
		l.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, l)
	}

	return out, total, rows.Err()
}

// ---------------------------------------------------------------- 设置

// GetSetting 读设置。
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting 写设置。
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
