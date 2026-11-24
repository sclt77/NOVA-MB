// Package runner 是面板唯一的命令执行入口。
//
// 安全模型（整个面板最关键的一段代码）：
//
//	面板不接受任何来自用户的命令字符串。所有能执行的动作都在这里
//	预先声明成 Action，每个 Action 的命令与参数在代码里写死。
//	用户能提供的只有「选择哪个动作」和「少量经过校验的参数」。
//
// 三条硬性约束：
//  1. 一律用 exec.Command(name, args...) 的参数数组形式，绝不拼字符串交给 shell。
//     只要出现 sh -c 就等于开了任意命令执行的口子。
//  2. 参数必须经过 ValidName / ValidPath 之类的校验，拒绝 ; | & $ ` 换行等字符。
//  3. 每个动作有超时，避免某个命令卡死把面板拖住。
package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Result 一次命令执行的结果。
type Result struct {
	Action   string   `json:"action"`
	Command  string   `json:"command"`  // 展示用，已脱敏
	Output   string   `json:"output"`   // 合并的 stdout + stderr
	ExitCode int      `json:"exitCode"`
	OK       bool     `json:"ok"`
	Duration float64  `json:"durationMs"`
	Warnings []string `json:"warnings,omitempty"`
}

// Action 一个允许执行的动作。
type Action struct {
	// Name 动作标识，前端按它调用
	Name string

	// Bin 可执行文件绝对路径。用绝对路径避免 PATH 劫持。
	Bin string

	// Args 固定参数。用户输入通过 ArgsFunc 追加。
	Args []string

	// ArgsFunc 根据用户输入生成额外参数。返回错误表示参数非法。
	// 为 nil 表示该动作不接受任何用户输入。
	ArgsFunc func(input map[string]string) ([]string, error)

	// Timeout 超时。0 用默认值。
	Timeout time.Duration

	// Display 展示给用户的命令描述（不含真实参数，便于审计阅读）
	Display string
}

// Runner 执行白名单动作。
type Runner struct {
	actions map[string]Action

	// sudo 前缀。面板以受限用户运行时需要它来执行 systemctl 等。
	// 为空表示直接执行（面板以 root 运行时）。
	sudo string
}

// New 创建 Runner。
//
// sudoBin 传 "sudo" 表示需要提权，传 "" 表示当前用户权限已够。
func New(sudoBin string) *Runner {
	return &Runner{
		actions: make(map[string]Action),
		sudo:    sudoBin,
	}
}

// Register 注册动作。同名重复注册会 panic——这是编码错误，越早暴露越好。
func (r *Runner) Register(a Action) {
	if _, exists := r.actions[a.Name]; exists {
		panic("runner: 重复注册动作 " + a.Name)
	}
	if a.Timeout == 0 {
		a.Timeout = 60 * time.Second
	}
	r.actions[a.Name] = a
}

// Has 判断动作是否存在。
func (r *Runner) Has(name string) bool {
	_, ok := r.actions[name]
	return ok
}

// Run 执行动作。
//
// input 只用于 ArgsFunc，不会进入 shell。
func (r *Runner) Run(name string, input map[string]string) (*Result, error) {
	a, ok := r.actions[name]
	if !ok {
		// 不暴露有哪些动作存在，避免探测
		return nil, fmt.Errorf("未知操作")
	}

	var extra []string
	if a.ArgsFunc != nil {
		var err error
		extra, err = a.ArgsFunc(input)
		if err != nil {
			return nil, err
		}
	} else if len(input) > 0 {
		// 动作不接受参数却传了参数，说明调用方有 bug 或有人在试探
		return nil, fmt.Errorf("该操作不接受参数")
	}

	args := append(append([]string{}, a.Args...), extra...)

	return r.exec(a, args)
}

func (r *Runner) exec(a Action, args []string) (*Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), a.Timeout)
	defer cancel()

	bin := a.Bin
	argv := args

	// 需要提权时把 sudo 放在最前面。
	// sudo 白名单在 /etc/sudoers.d/nova-panel 里限定到具体命令，
	// 所以这里即使想执行别的命令也会被 sudo 拒绝。
	if r.sudo != "" {
		argv = append([]string{bin}, args...)
		bin = r.sudo
	}

	cmd := exec.CommandContext(ctx, bin, argv...)

	// 不设置 Dir / Env：继承面板自己的环境，避免把用户输入混进去
	start := time.Now()
	out, err := cmd.CombinedOutput()
	elapsed := time.Since(start)

	res := &Result{
		Action:   a.Name,
		Command:  displayCommand(a, args),
		Output:   truncate(string(out), 64*1024),
		Duration: float64(elapsed.Milliseconds()),
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			res.OK = false
			return res, nil // 命令跑了但返回非 0，不算框架错误
		}

		if ctx.Err() == context.DeadlineExceeded {
			res.Output += fmt.Sprintf("\n[超时] 命令执行超过 %s 被终止", a.Timeout)
			res.ExitCode = -1
			res.OK = false
			return res, nil
		}

		return nil, fmt.Errorf("执行失败: %w", err)
	}

	res.ExitCode = 0
	res.OK = true
	return res, nil
}

func displayCommand(a Action, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, a.Bin)
	parts = append(parts, args...)

	s := strings.Join(parts, " ")
	if a.Display != "" {
		return a.Display
	}
	return s
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n...（输出过长，已截断，原长 %d 字节）", len(s))
}

// ---------------------------------------------------------------- 输入校验

var (
	// 服务名、动作名等：只允许字母数字和连字符
	reName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

	// 域名：标准 DNS 规则，允许泛域名前缀
	reDomain = regexp.MustCompile(`^(\*\.)?([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,24}$`)

	// 备份时间戳：20261124-103000
	reStamp = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)

	// 版本目录名同上
	reRelease = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)

	// 邮箱（申请证书用）
	reEmail = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// ValidName 校验服务名、标识符。
func ValidName(s string) bool {
	return reName.MatchString(s)
}

// ValidDomain 校验域名。
func ValidDomain(s string) bool {
	if len(s) > 253 {
		return false
	}
	return reDomain.MatchString(s)
}

// ValidStamp 校验备份时间戳。
func ValidStamp(s string) bool {
	return reStamp.MatchString(s)
}

// ValidRelease 校验版本目录名。
func ValidRelease(s string) bool {
	return reRelease.MatchString(s)
}

// ValidEmail 校验邮箱。
func ValidEmail(s string) bool {
	return len(s) <= 254 && reEmail.MatchString(s)
}

// ValidLogName 校验日志文件名。
//
// 日志文件是面板暴露给用户查看的内容，必须严格限制在白名单里，
// 否则就成了任意文件读取漏洞。
func ValidLogName(s string) bool {
	switch s {
	case "nova-api.log", "install.log", "cron.log",
		"nova-api", "nginx-access", "nginx-error",
		"panel":
		return true
	}
	return false
}

// ContainsShellMeta 检测字符串里是否含 shell 元字符。
//
// 虽然我们的执行路径不走 shell，这个函数仍然保留作为第二道防线：
// 万一以后有人加了 sh -c，这些字符就是攻击入口。
func ContainsShellMeta(s string) bool {
	const meta = ";|&$`\n\r\t\\\"'<>(){}[]*?!~#"
	return strings.ContainsAny(s, meta)
}
