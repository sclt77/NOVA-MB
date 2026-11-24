//go:build linux || darwin

package service

import "syscall"

// readDisk 读磁盘使用情况。
//
// 单独按平台分文件：syscall.Statfs_t 在 Windows 上不存在，
// 混在一个文件里就没法在 Windows 上编译（本地开发需要能编译验证）。
func readDisk(path string) (totalMB, usedMB int64, percent int) {
	var st syscall.Statfs_t

	if err := syscall.Statfs(path, &st); err != nil {
		// 路径不存在时退回根分区
		if err := syscall.Statfs("/", &st); err != nil {
			return 0, 0, 0
		}
	}

	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)

	totalMB = int64(total / 1024 / 1024)
	usedMB = int64((total - free) / 1024 / 1024)

	if total > 0 {
		percent = int((total - free) * 100 / total)
	}

	return
}
