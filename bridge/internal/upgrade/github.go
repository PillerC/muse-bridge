package upgrade

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 默认仓库与资产命名。
//
// 资产名必须与 .github/workflows/release.yml 里产出的一致：
// bridge-<os>-<arch>[.exe]，另有一个 checksums.txt（sha256sum 格式）。
const (
	DefaultRepo   = "goojia/muse-bridge"
	DefaultAPIURL = "https://api.github.com"

	// maxAssetSize 限制下载体积，防止被伪造的 Release 撑爆磁盘。
	// 真实二进制约 14MB，给到 64MB 已经很宽松。
	maxAssetSize = 64 << 20
	// maxChecksumSize checksums.txt 只有几百字节。
	maxChecksumSize = 256 << 10
)

// Release 是一次检查的结果，既用于展示也用于下载。
type Release struct {
	Tag         string    `json:"tag"`
	Name        string    `json:"name"`
	Notes       string    `json:"notes"`
	PublishedAt time.Time `json:"published_at"`
	PageURL     string    `json:"page_url"`
	AssetURL    string    `json:"asset_url"`
	AssetName   string    `json:"asset_name"`
	ChecksumURL string    `json:"checksum_url"`
	// Size 是资产字节数，仅用于展示。
	Size int64 `json:"size"`
}

// GitHubRelease 是 GitHub API 返回的子集。
type GitHubRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	HTMLURL     string `json:"html_url"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

// Options 是升级器需要的全部配置。
type Options struct {
	// Repo 形如 goojia/muse-bridge。
	Repo string
	// APIBase 允许指向 GitHub Enterprise 或测试用的假服务。
	APIBase string
	// Channel 是 release 或 prerelease：是否接受预发布版本。
	Channel string
	// HTTP 用于所有出站请求，超时由调用方设定。
	HTTP *http.Client
	// GOOS / GOARCH 默认取运行时值，测试里可以覆盖。
	GOOS   string
	GOARCH string
	// Current 是当前版本，用于比较与展示。
	Current string
	// Commit 是构建提交哈希，可选，只用于 User-Agent。
	Commit string
}

// Client 查询 GitHub Releases 并下载升级包。
type Client struct {
	opt Options
}

// NewClient 构造客户端，缺省字段用运行时值补齐。
func NewClient(opt Options) *Client {
	if opt.Repo == "" {
		opt.Repo = DefaultRepo
	}
	if opt.APIBase == "" {
		opt.APIBase = DefaultAPIURL
	}
	opt.APIBase = strings.TrimRight(opt.APIBase, "/")
	if opt.Channel == "" {
		opt.Channel = ChannelRelease
	}
	if opt.HTTP == nil {
		opt.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if opt.GOOS == "" {
		opt.GOOS = runtimeGOOS()
	}
	if opt.GOARCH == "" {
		opt.GOARCH = runtimeGOARCH()
	}
	return &Client{opt: opt}
}

// userAgent 拼请求头。GitHub 要求带 UA，否则直接 403。
func (c *Client) userAgent() string {
	ua := "muse-bridge/" + c.opt.Current
	if c.opt.Commit != "" {
		ua += " (" + c.opt.Commit + ")"
	}
	return ua
}

// AssetName 返回当前平台对应的资产名。
func (c *Client) AssetName() string {
	return AssetName(c.opt.GOOS, c.opt.GOARCH)
}

// AssetName 按平台拼资产名，与发布流水线保持一致。
func AssetName(goos, goarch string) string {
	name := "bridge-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// Check 查询最新可用版本。
//
// 返回 (nil, nil) 表示"没有找到适合当前平台的发布"，不是错误：
// 仓库还没发过版、或者这个平台没出包，都属于正常情况，页面上提示即可。
func (c *Client) Check(ctx context.Context) (*Release, error) {
	if c.opt.GOOS == "windows" {
		// Windows 上无法用 exec 自替换重启，也没人这么部署；直接不支持，
		// 免得用户点了升级把服务弄成半坏状态。
		return nil, ErrUnsupportedPlatform
	}
	list, err := c.listReleases(ctx)
	if err != nil {
		return nil, err
	}
	want := c.AssetName()
	for _, rel := range list {
		if rel.Draft {
			continue
		}
		if rel.Prerelease && c.opt.Channel != ChannelPrerelease {
			continue
		}
		var assetURL string
		var assetSize int64
		var checksumURL string
		for _, a := range rel.Assets {
			switch a.Name {
			case want:
				assetURL, assetSize = a.BrowserDownloadURL, a.Size
			case "checksums.txt":
				checksumURL = a.BrowserDownloadURL
			}
		}
		if assetURL == "" {
			continue // 这一版没为当前平台出包，看下一版
		}
		pub, _ := time.Parse(time.RFC3339, rel.PublishedAt)
		name := rel.Name
		if name == "" {
			name = rel.TagName
		}
		return &Release{
			Tag:         rel.TagName,
			Name:        name,
			Notes:       rel.Body,
			PublishedAt: pub,
			PageURL:     rel.HTMLURL,
			AssetURL:    assetURL,
			AssetName:   want,
			ChecksumURL: checksumURL,
			Size:        assetSize,
		}, nil
	}
	return nil, nil
}

func (c *Client) listReleases(ctx context.Context) ([]GitHubRelease, error) {
	url := c.opt.APIBase + "/repos/" + c.opt.Repo + "/releases?per_page=20"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.opt.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("仓库 %s 不存在或没有发布记录", c.opt.Repo)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub 接口限流（HTTP %d），请稍后再试", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub 接口返回 HTTP %d", resp.StatusCode)
	}

	var out []GitHubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析 GitHub 响应失败: %w", err)
	}
	return out, nil
}

// Download 把资产下载到 dst 并返回字节数。调用方负责之后校验摘要。
func (c *Client) Download(ctx context.Context, url string, dst io.Writer) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.opt.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	n, err := io.Copy(dst, io.LimitReader(resp.Body, maxAssetSize+1))
	if err != nil {
		return n, fmt.Errorf("写入下载内容失败: %w", err)
	}
	if n > maxAssetSize {
		return n, fmt.Errorf("升级包超过 %d 字节，已中止", maxAssetSize)
	}
	return n, nil
}

// FetchChecksums 下载 checksums.txt 并解析成 文件名 -> sha256 的映射。
func (c *Client) FetchChecksums(ctx context.Context, url string) (map[string]string, error) {
	if url == "" {
		return nil, errors.New("该发布没有 checksums.txt，无法校验升级包完整性")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := c.opt.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载校验文件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载校验文件失败: HTTP %d", resp.StatusCode)
	}

	out := map[string]string{}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, maxChecksumSize))
	for sc.Scan() {
		// 格式：<sha256>  <文件名>（sha256sum 输出，两空格）
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		out[name] = strings.ToLower(fields[0])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取校验文件失败: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("校验文件内容为空")
	}
	return out, nil
}
