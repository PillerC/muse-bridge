// Package upgrade 实现在线升级：查 GitHub Releases、下载对应平台的二进制、
// 校验 sha256、原子替换自身文件，然后原地 exec 换成新版本。
//
// 三条设计约束决定了这里的写法：
//
//  1. 升级包只从 GitHub Releases 拿，用户机器不需要 Go 工具链和 git；
//  2. 必须校验 sha256 —— 下载的是要直接执行的文件，不能只靠 HTTPS 就信；
//  3. 替换与重启都要么成功要么回到原样：先写到同目录的临时文件再 rename，
//     rename 在同文件系统上是原子的，中途断电也不会留下半个二进制。
package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// 升级通道。
const (
	ChannelRelease    = "release"
	ChannelPrerelease = "prerelease"
)

var (
	// ErrUnsupportedPlatform 表示当前平台不支持在线升级（Windows）。
	ErrUnsupportedPlatform = errors.New("当前平台不支持在线升级")
	// ErrChecksumMismatch 表示下载内容的摘要与 checksums.txt 不符。
	ErrChecksumMismatch = errors.New("升级包校验失败：sha256 与发布信息不符")
	// ErrNotWritable 表示二进制所在目录不可写，无法替换自身。
	ErrNotWritable = errors.New("二进制所在目录不可写，无法自动升级")
)

func runtimeGOOS() string   { return runtime.GOOS }
func runtimeGOARCH() string { return runtime.GOARCH }

// SelfPath 返回当前可执行文件的真实路径（解开符号链接）。
//
// 用 os.Executable 而不是 os.Args[0]：后者可能是相对路径，也可能是通过
// PATH 找到的名字；升级要替换的是真实文件，不是调用方式。
func SelfPath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("无法定位当前可执行文件: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return p, nil
}

// Install 把 r 里的升级包写到 target，返回备份文件路径。
//
// 流程：同目录临时文件 → 校验摘要 → 赋予可执行位 → 备份旧文件 → rename。
// 任何一步失败都会清掉临时文件，旧二进制保持不动。
func Install(r io.Reader, target, wantSHA string) (backup string, err error) {
	dir := filepath.Dir(target)
	// 先确认目录可写，免得辛苦下载完才失败。
	if err := checkWritable(dir); err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(dir, ".bridge-upgrade-*")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotWritable, err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("写入升级包失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("落盘失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if n == 0 {
		os.Remove(tmpName)
		return "", errors.New("升级包为空")
	}

	got := hex.EncodeToString(h.Sum(nil))
	if wantSHA != "" && !equalHex(got, wantSHA) {
		os.Remove(tmpName)
		return "", fmt.Errorf("%w（期望 %s，实际 %s）", ErrChecksumMismatch, wantSHA, got)
	}

	if err := os.Chmod(tmpName, 0o755); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("设置可执行权限失败: %w", err)
	}

	// 备份旧文件：rename 之后旧文件就没了，留一份以便回滚。
	// 失败不算致命（比如目录不可写之外的怪情况），继续升级更符合预期。
	if err := copyFile(target, target+".bak"); err == nil {
		backup = target + ".bak"
	}

	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return backup, fmt.Errorf("替换可执行文件失败: %w", err)
	}
	return backup, nil
}

// InstallFile 把一个已经落盘的升级包（src）校验后替换到 target。
//
// 与 Install 的区别是不再复制一遍内容：升级包下载完就已经在目标目录里，
// 校验通过直接 rename 即可，省掉一次 14MB 的写入。
func InstallFile(src, target, wantSHA string) (backup string, err error) {
	got, err := fileSHA(src)
	if err != nil {
		return "", err
	}
	if wantSHA != "" && !equalHex(got, wantSHA) {
		return "", fmt.Errorf("%w（期望 %s，实际 %s）", ErrChecksumMismatch, wantSHA, got)
	}
	if err := os.Chmod(src, 0o755); err != nil {
		return "", fmt.Errorf("设置可执行权限失败: %w", err)
	}
	// 备份旧文件：rename 之后旧文件就没了，留一份以便回滚。
	if err := copyFile(target, target+".bak"); err == nil {
		backup = target + ".bak"
	}
	if err := os.Rename(src, target); err != nil {
		return backup, fmt.Errorf("替换可执行文件失败: %w", err)
	}
	return backup, nil
}

// fileSHA 算文件的 sha256。
func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// checkWritable 用临时文件探一下目录能不能写。
//
// 只看权限位不够：只读挂载、容器里的只读层、SELinux 都可能让 os.Rename 失败，
// 而下载 14MB 之后再失败是很差的体验。
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".bridge-writable-*")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotWritable, err)
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

// copyFile 复制文件内容与权限位。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// equalHex 比较两个十六进制摘要，忽略大小写与空白。
func equalHex(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// NewHash 供调用方边下边算摘要（例如先下载到文件再校验的场景）。
func NewHash() hash.Hash { return sha256.New() }

// SumHex 返回 hash 的十六进制摘要。
func SumHex(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }
