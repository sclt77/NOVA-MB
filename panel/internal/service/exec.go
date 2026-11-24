package service

import (
	"context"
	"os/exec"
	"time"
)

// execCommandContext 是 exec.CommandContext 的薄封装。
//
// 单独抽出来是为了让「命令执行」在代码里只有一处入口，
// 便于审查：搜 execCommandContext 就能看到所有命令调用点。
func execCommandContext(ctx context.Context, bin string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, bin, args...)
}

// runWithTimeout 带超时执行命令，返回合并输出。
func runWithTimeout(timeout time.Duration, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := execCommandContext(ctx, bin, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
