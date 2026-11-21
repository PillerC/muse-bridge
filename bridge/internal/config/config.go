package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 是进程启动所需的全部配置。
// 所有字段在启动时校验完毕，运行期不再读环境变量——
// 缺失配置直接退出，不允许带着半套配置跑起来（v1 的 webhook 鉴权 fail-open 就是这么来的）。
type Config struct {
	// Addr 是管理后台的监听地址。默认只绑本地回环，通过 SSH 隧道访问。
	Addr string
	// PublicAddr 是公网监听地址，只暴露 webhook 和 Muse API。
	PublicAddr string
	// PublicURL 是外部可访问的 https 基址，用于拼 webhook 地址。
	PublicURL string
	// TLSMode 是 autocert | cert | off。
	TLSMode string
	// ACMEEmail 是 Let's Encrypt 账号邮箱。
	ACMEEmail string
	// TLSCert / TLSKey 在 TLSMode=cert 时使用。
	TLSCert string
	TLSKey  string
	// DataDir 放数据库、日志、证书缓存。
	DataDir string
	// AdminUser / AdminPassword 用于创建或同步管理员密码。
	AdminUser     string
	AdminPassword string
	// Secret 是 32 字节主密钥，用于加密机器人 token。
	Secret []byte
	// DefaultMode 是新桥的默认接入方式：poll 或 webhook。
	DefaultMode string
	// LeaseSeconds 是认领默认租约。
	LeaseSeconds int
	// MaxAttempts 超过后进死信。
	MaxAttempts int
	// TGProxy 是访问 api.telegram.org 的 HTTP 代理，可留空。
	TGProxy string
}

// DBPath 返回 SQLite 文件路径。
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "bridge.db") }

// CertDir 返回 autocert 的证书缓存目录。
func (c *Config) CertDir() string { return filepath.Join(c.DataDir, "certs") }

// Host 返回 PublicURL 的主机名，供 autocert 白名单使用。
func (c *Config) Host() string {
	u := strings.TrimPrefix(strings.TrimPrefix(c.PublicURL, "https://"), "http://")
	if i := strings.IndexAny(u, "/:"); i >= 0 {
		return u[:i]
	}
	return u
}

// Load 读取环境变量并校验。任何一项不合法都返回错误，调用方应直接退出。
func Load() (*Config, error) {
	dataDir := env("BRIDGE_DATA_DIR", "/data")
	c := &Config{
		Addr:          env("BRIDGE_ADDR", "127.0.0.1:8080"),
		PublicAddr:    env("BRIDGE_PUBLIC_ADDR", "0.0.0.0:443"),
		PublicURL:     strings.TrimRight(env("BRIDGE_PUBLIC_URL", ""), "/"),
		ACMEEmail:     env("BRIDGE_ACME_EMAIL", ""),
		TLSCert:       env("BRIDGE_TLS_CERT", ""),
		TLSKey:        env("BRIDGE_TLS_KEY", ""),
		DataDir:       dataDir,
		AdminUser:     env("BRIDGE_ADMIN_USER", "admin"),
		AdminPassword: os.Getenv("BRIDGE_ADMIN_PASSWORD"),
		DefaultMode:   env("BRIDGE_MODE", "webhook"),
		LeaseSeconds:  180,
		MaxAttempts:   5,
		TGProxy:       env("BRIDGE_TG_PROXY", ""),
	}

	secretRaw := os.Getenv("BRIDGE_SECRET")
	if secretRaw == "" {
		return nil, errors.New("BRIDGE_SECRET 未设置：它是加密机器人 token 的主密钥，必须提供")
	}
	secret, err := base64.StdEncoding.DecodeString(secretRaw)
	if err != nil {
		return nil, fmt.Errorf("BRIDGE_SECRET 不是合法 base64: %w", err)
	}
	if len(secret) != 32 {
		return nil, fmt.Errorf("BRIDGE_SECRET 解码后必须是 32 字节，当前 %d 字节", len(secret))
	}
	c.Secret = secret

	if c.AdminPassword == "" {
		return nil, errors.New("BRIDGE_ADMIN_PASSWORD 未设置：首次启动需要它来创建管理员")
	}
	if c.DefaultMode != "poll" && c.DefaultMode != "webhook" {
		return nil, fmt.Errorf("BRIDGE_MODE 只能是 poll 或 webhook，当前 %q", c.DefaultMode)
	}

	// TLS 模式：显式配置优先，否则按 PublicURL 推断。
	c.TLSMode = env("BRIDGE_TLS", "")
	if c.TLSMode == "" {
		if strings.HasPrefix(c.PublicURL, "https://") {
			c.TLSMode = "autocert"
		} else {
			c.TLSMode = "off"
		}
	}
	switch c.TLSMode {
	case "autocert":
		if c.Host() == "" {
			return nil, errors.New("autocert 模式必须设置 BRIDGE_PUBLIC_URL（例如 https://bridge.example.com）")
		}
		if c.PublicAddr == "" || strings.HasSuffix(c.PublicAddr, ":443") {
			// autocert 的 HTTP-01 需要 80 端口，主监听仍在 443。
		}
	case "cert":
		if c.TLSCert == "" || c.TLSKey == "" {
			return nil, errors.New("TLSMode=cert 需要 BRIDGE_TLS_CERT 和 BRIDGE_TLS_KEY")
		}
	case "off":
		if c.PublicURL == "" {
			return nil, errors.New("未配置 BRIDGE_PUBLIC_URL：Muse 需要一个可访问的地址")
		}
	default:
		return nil, fmt.Errorf("BRIDGE_TLS 只能是 autocert / cert / off，当前 %q", c.TLSMode)
	}

	if c.DefaultMode == "webhook" && c.PublicURL == "" {
		return nil, errors.New("webhook 模式必须设置 BRIDGE_PUBLIC_URL（Telegram 不允许 IP 直连且证书必须有效）")
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("无法创建数据目录 %s: %w", dataDir, err)
	}
	return c, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
