//go:build windows

package service

// readDisk Windows 下的占位实现。
//
// 面板只跑在 Linux 上，Windows 版本仅用于本地开发时编译验证，
// 所以这里返回 0 就够了，不引入 Windows 的 API 调用。
func readDisk(path string) (totalMB, usedMB int64, percent int) {
	return 0, 0, 0
}
