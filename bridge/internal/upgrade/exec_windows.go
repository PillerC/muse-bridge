//go:build windows

package upgrade

// Restart 在 Windows 上不支持：运行中的 exe 被独占锁定，无法 rename 覆盖，
// 也没有 exec 可以原地换映像。Check 在 Windows 上会提前返回
// ErrUnsupportedPlatform，这个函数只是为了让包能交叉编译通过。
func Restart() error { return ErrUnsupportedPlatform }
