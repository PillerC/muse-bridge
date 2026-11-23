//go:build !windows

package upgrade

import (
	"os"
	"syscall"
)

// Restart 用新二进制原地替换当前进程。
//
// 为什么是 exec 而不是 exit(0) 靠服务管理器拉起：
//   - exec 保留 PID、文件描述符和 cgroup 归属，systemd/OpenRC/Docker 全都不用改配置；
//   - exit(0) 依赖 Restart=always 这类配置，用户手搓的启动方式（nohup、screen）
//     一旦没有自动重启，升级就变成"服务停了"。
//
// exec 成功时不会返回；返回错误说明替换失败，调用方可以记录并继续跑旧版本。
func Restart() error {
	exe, err := SelfPath()
	if err != nil {
		return err
	}
	// argv[0] 用绝对路径而不是原来的 os.Args[0]：原来是相对路径的话，
	// exec 之后工作目录未必还指向同一处。
	argv := append([]string{exe}, os.Args[1:]...)
	return syscall.Exec(exe, argv, os.Environ())
}
