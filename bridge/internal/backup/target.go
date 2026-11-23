// Package backup 把每日归档上传到远端存储。
//
// 支持两类目标：S3 兼容对象存储（AWS S3 / MinIO / Cloudflare R2 / 阿里云 OSS /
// 腾讯云 COS 等）和 WebDAV 网盘（Nextcloud / 坚果云 / 群晖等）。
//
// 两类目标只用标准库实现：S3 走自己实现的 SigV4 签名，WebDAV 走 net/http。
// 不引 AWS SDK 是刻意的——它会给单文件二进制增加十几 MB，而这台机器
// 内存和磁盘都很紧，功能上我们也只需要 PUT / DELETE / 探测三件事。
package backup

import (
	"context"
	"errors"
	"fmt"
)

// 目标类型。
const (
	TypeS3     = "s3"
	TypeWebDAV = "webdav"
)

// ErrNotConfigured 表示还没配置备份目标。
var ErrNotConfigured = errors.New("还没有配置备份目标")

// Target 是一个备份目标。
//
// 只用文件路径而不是 io.Reader：归档文件就在本地磁盘上，
// 上传时要拿长度、要算摘要、失败要重试，给路径比给流简单得多。
type Target interface {
	// Name 返回给人看的名字，例如 "S3: my-bucket/muse-bridge"。
	Name() string
	// Put 把本地文件上传到 key。已存在则覆盖。
	Put(ctx context.Context, key, localPath string) error
	// Delete 删除 key。对象不存在不视为错误。
	Delete(ctx context.Context, key string) error
	// Ping 验证配置是否可用，供后台「测试连接」用。
	Ping(ctx context.Context) error
}

// S3Config 是 S3 兼容存储的配置。
type S3Config struct {
	// Endpoint 形如 https://s3.us-east-1.amazonaws.com 或 https://minio.example.com:9000。
	// 不带 scheme 时按 https 处理。
	Endpoint string
	// Region 区域，MinIO 等可留空，默认 us-east-1。
	Region string
	Bucket string
	// Prefix 是 bucket 内的路径前缀，可留空。
	Prefix string
	// PathStyle 为 true 时用 path-style 寻址（MinIO、部分自建服务需要），
	// 否则用 virtual-hosted-style（AWS 默认）。
	PathStyle    bool
	AccessKey    string
	SecretKey    string
	SessionToken string
}

// WebDAVConfig 是 WebDAV 的配置。
type WebDAVConfig struct {
	// URL 是目录地址，形如 https://dav.example.com/remote.php/dav/files/user/bridge/
	URL      string
	Username string
	Password string
}

// Config 是持久化的备份与保留配置（凭据已解密）。
type Config struct {
	Type   string
	S3     S3Config
	WebDAV WebDAVConfig
	// Prefix 是所有目标共用的对象键前缀，用来把多台实例的数据分开。
	Prefix string
	// Enabled 表示是否参与定时上传。
	Enabled bool
	// KeepDays 是本地聊天记录保留天数，0 表示永不清理。
	KeepDays int
	// Retain 表示是否启用本地清理。
	Retain bool
}

// Validate 检查配置完整性，返回中文错误。
func (c Config) Validate() error {
	switch c.Type {
	case TypeS3:
		if c.S3.Endpoint == "" {
			return errors.New("S3 Endpoint 不能为空")
		}
		if c.S3.Bucket == "" {
			return errors.New("S3 Bucket 不能为空")
		}
		if c.S3.AccessKey == "" || c.S3.SecretKey == "" {
			return errors.New("S3 Access Key 和 Secret Key 都不能为空")
		}
	case TypeWebDAV:
		if c.WebDAV.URL == "" {
			return errors.New("WebDAV 地址不能为空")
		}
	case "":
		return ErrNotConfigured
	default:
		return fmt.Errorf("未知的备份类型 %q", c.Type)
	}
	return nil
}

// New 按配置构造目标。
func New(cfg Config) (Target, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	switch cfg.Type {
	case TypeS3:
		return NewS3(cfg.S3, cfg.Prefix)
	case TypeWebDAV:
		return NewWebDAV(cfg.WebDAV, cfg.Prefix)
	default:
		return nil, fmt.Errorf("未知的备份类型 %q", cfg.Type)
	}
}

// joinKey 拼对象键，处理前缀与分隔符。
func joinKey(prefix, name string) string {
	prefix = trimSlashes(prefix)
	name = trimSlashes(name)
	if prefix == "" {
		return name
	}
	if name == "" {
		return prefix
	}
	return prefix + "/" + name
}

// trimSlashes 去掉首尾多余的斜杠。
func trimSlashes(s string) string {
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
