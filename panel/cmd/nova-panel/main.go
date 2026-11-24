// Nova 部署面板
//
// 一个专为 Nova 项目设计的运维面板：部署、备份、域名、日志、服务控制。
//
// 设计原则见 docs/ARCHITECTURE.md。核心是「白名单动作」而不是通用 shell：
// 面板能做的事在 internal/runner/actions.go 里穷举列出，没有自由输入命令的入口。
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sclt77/nova-mb/internal/api"
	"github.com/sclt77/nova-mb/internal/auth"
	"github.com/sclt77/nova-mb/internal/config"
	"github.com/sclt77/nova-mb/internal/runner"
	"github.com/sclt77/nova-mb/internal/store"
)

//go:embed web
var webFS embed.FS

// 版本号，编译时通过 -ldflags 注入
var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	var (
		cfgPath     = flag.String("config", "", "配置文件路径")
		showVersion = flag.Bool("version", false, "显示版本")
		resetPass   = flag.Bool("reset-password", false, "重置管理员密码（需先停止面板服务）")
		listenAddr  = flag.String("listen", "", "覆盖监听地址，如 0.0.0.0")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("nova-panel %s (built %s)\n", version, buildTime)
		return
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	if *listenAddr != "" {
		cfg.Listen = *listenAddr
	}

	// 数据存储
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("打开数据存储失败: %v", err)
	}
	defer st.Close()

	// 首次启动：创建管理员账号并打印初始密码
	if err := bootstrapAdmin(st, *resetPass); err != nil {
		log.Fatalf("初始化管理员失败: %v", err)
	}
	if *resetPass {
		return
	}

	// 认证
	am := auth.New(st, cfg.UseTLS)

	// 命令执行器：需要 sudo 时传 "sudo"，以 root 运行时传空
	sudoBin := ""
	if os.Geteuid() != 0 {
		sudoBin = "sudo"
	}
	rn := runner.New(sudoBin)
	runner.RegisterDefaults(rn, cfg)

	// 静态资源
	static, err := staticHandler()
	if err != nil {
		log.Fatalf("加载静态资源失败: %v", err)
	}

	// HTTP 服务
	srv := api.New(cfg, st, am, rn)

	// 优雅关闭
	done := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("收到退出信号，正在关闭…")
		close(done)
	}()

	go func() {
		if err := srv.Run(static); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务异常退出: %v", err)
		}
	}()

	log.Printf("Nova 面板 %s 已启动", version)

	<-done

	// 给在途请求 10 秒收尾
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ctx

	log.Println("已停止")
}

// bootstrapAdmin 首次启动时创建管理员，或按参数重置密码。
func bootstrapAdmin(st *store.Store, reset bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const defaultUser = "admin"

	if reset {
		// 重置模式：生成新密码并打印
		admin, err := st.AdminByUsername(ctx, defaultUser)
		if err != nil {
			return fmt.Errorf("找不到管理员账号 %s", defaultUser)
		}

		pass, err := config.RandomPassword(16)
		if err != nil {
			return err
		}

		hash, err := auth.HashPassword(pass)
		if err != nil {
			return err
		}

		if err := st.UpdatePassword(ctx, admin.ID, hash); err != nil {
			return err
		}

		fmt.Printf("\n管理员密码已重置\n  用户名：%s\n  新密码：%s\n\n", defaultUser, pass)
		return nil
	}

	count, err := st.CountAdmins(ctx)
	if err != nil {
		return err
	}

	if count > 0 {
		return nil
	}

	// 首次启动
	pass, err := config.RandomPassword(16)
	if err != nil {
		return err
	}

	hash, err := auth.HashPassword(pass)
	if err != nil {
		return err
	}

	if _, err := st.CreateAdmin(ctx, defaultUser, hash); err != nil {
		return err
	}

	// 把初始密码写到文件，权限 600。
	// 同时打印到终端——首次启动时用户正看着日志。
	passFile := filepath.Join(os.Getenv("NOVA_PANEL_DATA_DIR"), "initial-password.txt")
	if passFile == "/initial-password.txt" {
		passFile = "/var/lib/nova/panel/initial-password.txt"
	}

	_ = os.MkdirAll(filepath.Dir(passFile), 0o700)
	_ = os.WriteFile(passFile,
		[]byte(fmt.Sprintf("用户名: %s\n密码: %s\n请登录后立即修改，并删除本文件。\n", defaultUser, pass)),
		0o600)

	fmt.Printf(`
════════════════════════════════════════
  面板首次启动，已创建管理员账号
    用户名：%s
    密码：  %s
  请登录后立即修改密码。
════════════════════════════════════════
`, defaultUser, pass)

	return nil
}

// staticHandler 返回静态文件服务。
//
// 单页应用需要「找不到文件时回落到 index.html」，但**只能对页面路由生效**。
// 静态资源（/static/、*.js、*.css）找不到时必须返回 404——
// 回落成 index.html 会让浏览器把 HTML 当成 CSS 解析，报一堆莫名其妙的错。
func staticHandler() (http.Handler, error) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}

		// 静态资源：直接交给 FileServer，找不到就是 404
		if isStaticAsset(path) {
			w.Header().Set("Cache-Control", "public, max-age=3600")
			fileServer.ServeHTTP(w, r)
			return
		}

		// 页面路由：文件存在就返回，不存在回落到 index.html
		if f, err := sub.Open(strings.TrimPrefix(path, "/")); err == nil {
			f.Close()
			if path == "/index.html" {
				w.Header().Set("Cache-Control", "no-store")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA 回落
		r.URL.Path = "/index.html"
		w.Header().Set("Cache-Control", "no-store")
		fileServer.ServeHTTP(w, r)
	}), nil
}

// isStaticAsset 判断是否静态资源路径。
func isStaticAsset(path string) bool {
	if strings.HasPrefix(path, "/static/") {
		return true
	}
	switch filepath.Ext(path) {
	case ".js", ".css", ".png", ".jpg", ".jpeg", ".gif", ".svg",
		".ico", ".woff", ".woff2", ".ttf", ".map":
		return true
	}
	return false
}
