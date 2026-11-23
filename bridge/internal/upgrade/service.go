// Package 内的升级服务：把 upgrade 包的下载能力、settings 表和定时器接起来。
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Settings 是升级相关的持久化配置，由 store 读写。
//
// 这里定义接口而不是直接依赖 *store.Store：升级服务只用到这几个读写，
// 用接口能让测试不建数据库，也避免 upgrade 包反向依赖 store。
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string, now int64) error
	SetSettings(ctx context.Context, values map[string]string, now int64) error
	DeleteSetting(ctx context.Context, key string) error
}

// 配置键。与 store.Setting* 常量同名同值，两边由调用方对齐。
const (
	KeyChannel      = "upgrade.channel"       // release | prerelease
	KeyAuto         = "upgrade.auto"          // 1 | 0
	KeyAt           = "upgrade.at"            // HH:MM
	KeyLastCheck    = "upgrade.last_check"    // unix 秒
	KeyLastVersion  = "upgrade.last_version"  // 最近看到的版本号
	KeyLastResult   = "upgrade.last_result"   // ok | error | none
	KeyLastMessage  = "upgrade.last_message"  // 结果说明
	KeyLastRun      = "upgrade.last_run"      // 最近一次升级动作时间
	KeyLastError    = "upgrade.last_error"    // 失败原因
	KeyPendingFrom  = "upgrade.pending_from"  // 升级前版本
	KeyPendingTo    = "upgrade.pending_to"    // 期望升级到的版本
	KeyStartupCheck = "upgrade.startup_check" // 1 | 0
	KeyLastFrom     = "upgrade.last_from"     // 最近一次成功升级的来源版本
	KeyLastTo       = "upgrade.last_to"       // 最近一次成功升级的目标版本
	KeyLastAutoDay  = "upgrade.last_auto_day" // 定时升级最近执行过的日期（YYYY-MM-DD）
)

// autoWindow 是定时升级的触发窗口。
//
// 触发条件是"今天到了设定时刻之后 30 分钟内"，而不是"分钟数正好相等"：
// 后台定时器 30 秒一轮，正好卡在那一分钟里才触发的话，
// 进程刚好在那一刻重启（比如刚升级完 exec 回来）就会整天跳过。
const autoWindow = 30 * time.Minute

// 升级结果。
const (
	ResultNone  = "none"
	ResultOK    = "ok"
	ResultError = "error"
)

// checkCacheTTL 是"不强制刷新"时复用上次检查结果的时间窗。
// 打开升级页不该每次都打 GitHub（有 60 次/小时的匿名限流）。
const checkCacheTTL = 10 * time.Minute

// Config 是当前生效的升级配置。
type Config struct {
	Channel      string `json:"channel"`
	Auto         bool   `json:"auto"`
	At           string `json:"at"` // HH:MM
	StartupCheck bool   `json:"startup_check"`
}

// DefaultConfig 返回默认配置：稳定通道、定时升级关闭、每天 04:00、启动时检查一次。
//
// 定时升级默认关闭是刻意的：自动重启服务这种事必须由用户自己打开，
// 不能装完就开始在半夜换掉人家正在跑的程序。
func DefaultConfig() Config {
	return Config{Channel: ChannelRelease, Auto: false, At: "04:00", StartupCheck: true}
}

// JustDone 描述"刚完成一次升级"，用于重启后给用户一个确认。
type JustDone struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Status 是给后台页面用的快照。
type Status struct {
	Current     string    `json:"current"`
	Config      Config    `json:"config"`
	Latest      *Release  `json:"latest,omitempty"`
	HasUpdate   bool      `json:"has_update"`
	LastCheck   int64     `json:"last_check"`
	LastResult  string    `json:"last_result"`
	LastMessage string    `json:"last_message"`
	LastRun     int64     `json:"last_run"`
	LastError   string    `json:"last_error"`
	Running     bool      `json:"running"`
	JustUpgrade *JustDone `json:"just_upgrade,omitempty"`
	SelfPath    string    `json:"self_path"`
	AssetName   string    `json:"asset_name"`
	Platform    string    `json:"platform"`
	CanUpgrade  bool      `json:"can_upgrade"`
}

// ErrBusy 表示已经有一次检查或升级在进行。
var ErrBusy = errors.New("已有升级任务在进行，请稍候")

// Service 管理升级的检查、执行与定时。
type Service struct {
	st   Settings
	opt  Options
	now  func() time.Time
	self string

	mu      sync.Mutex
	running bool
	// restart 默认是 Restart（exec 自身），测试里可以替换成桩。
	restart func() error
	// latest 缓存最近一次检查结果，供页面直接展示。
	latest      *Release
	lastCheckAt time.Time
	// onBeforeRestart 在 exec 之前调用，用于停掉 typing 之类的收尾。
	onBeforeRestart func()
}

// NewService 构造升级服务。opt.Current 由调用方注入版本号。
func NewService(st Settings, opt Options) *Service {
	self, err := SelfPath()
	if err != nil {
		// 拿不到自身路径不影响查看配置，只是不能升级；操作时再报错。
		self = ""
	}
	return &Service{st: st, opt: opt, now: time.Now, self: self, restart: Restart}
}

// SetRestartForTest 替换重启动作，仅供测试使用。
func (s *Service) SetRestartForTest(fn func() error) { s.restart = fn }

// SetSelfPathForTest 替换自身路径，仅供测试使用。
func (s *Service) SetSelfPathForTest(p string) { s.self = p }

// SetNowForTest 替换时钟，仅供测试使用。
func (s *Service) SetNowForTest(fn func() time.Time) { s.now = fn }

// SetBeforeRestart 注册重启前的收尾回调。
func (s *Service) SetBeforeRestart(fn func()) { s.onBeforeRestart = fn }

// Current 返回当前版本。
func (s *Service) Current() string { return s.opt.Current }

// SelfPath 返回当前可执行文件路径。
func (s *Service) SelfPath() string { return s.self }

// LoadConfig 从库里读配置，缺失的项用默认值。
func (s *Service) LoadConfig(ctx context.Context) (Config, error) {
	cfg := DefaultConfig()
	get := func(k string) string {
		v, err := s.st.GetSetting(ctx, k)
		if err != nil {
			return ""
		}
		return v
	}
	if v := get(KeyChannel); v == ChannelPrerelease || v == ChannelRelease {
		cfg.Channel = v
	}
	cfg.Auto = get(KeyAuto) == "1"
	if v := get(KeyAt); v != "" && validClock(v) {
		cfg.At = v
	}
	// startup_check 默认开：从没写过这个键时读到空串。
	cfg.StartupCheck = get(KeyStartupCheck) != "0"
	return cfg, nil
}

// SaveConfig 校验并保存配置。
func (s *Service) SaveConfig(ctx context.Context, cfg Config) error {
	if cfg.Channel != ChannelRelease && cfg.Channel != ChannelPrerelease {
		return errors.New("升级通道只能是 release 或 prerelease")
	}
	if !validClock(cfg.At) {
		return errors.New("定时升级时间格式应为 HH:MM（00:00–23:59）")
	}
	return s.st.SetSettings(ctx, map[string]string{
		KeyChannel:      cfg.Channel,
		KeyAuto:         boolStr(cfg.Auto),
		KeyAt:           cfg.At,
		KeyStartupCheck: boolStr(cfg.StartupCheck),
	}, s.now().Unix())
}

// validClock 校验 HH:MM。
func validClock(v string) bool {
	if len(v) != 5 || v[2] != ':' {
		return false
	}
	h, err1 := strconv.Atoi(v[:2])
	m, err2 := strconv.Atoi(v[3:])
	if err1 != nil || err2 != nil {
		return false
	}
	return h >= 0 && h < 24 && m >= 0 && m < 60
}

func boolStr(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// Check 查询最新版本并落库结果。force=false 时复用十分钟内的检查结果。
func (s *Service) Check(ctx context.Context, force bool) (*Status, error) {
	cfg, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !force {
		if rel := s.cachedRelease(); rel != nil && s.now().Sub(s.cachedCheckAt()) < checkCacheTTL {
			return s.statusFrom(ctx, cfg, rel), nil
		}
	}
	if !s.acquire() {
		return nil, ErrBusy
	}
	defer s.release()
	return s.checkLocked(ctx, cfg)
}

func (s *Service) checkLocked(ctx context.Context, cfg Config) (*Status, error) {
	now := s.now()
	rel, err := s.client(cfg).Check(ctx)
	if err != nil {
		msg := err.Error()
		_ = s.st.SetSettings(ctx, map[string]string{
			KeyLastCheck:   strconv.FormatInt(now.Unix(), 10),
			KeyLastResult:  ResultError,
			KeyLastMessage: "检查更新失败：" + msg,
			KeyLastError:   msg,
		}, now.Unix())
		return nil, err
	}

	s.mu.Lock()
	s.latest = rel
	s.lastCheckAt = now
	s.mu.Unlock()

	values := map[string]string{
		KeyLastCheck:  strconv.FormatInt(now.Unix(), 10),
		KeyLastResult: ResultOK,
		KeyLastError:  "",
	}
	switch {
	case rel == nil:
		values[KeyLastMessage] = "没有找到适合当前平台的发布"
	case IsNewer(s.opt.Current, rel.Tag):
		values[KeyLastMessage] = "发现新版本 " + rel.Tag
		values[KeyLastVersion] = rel.Tag
	default:
		values[KeyLastMessage] = "已是最新版本"
		values[KeyLastVersion] = rel.Tag
	}
	if err := s.st.SetSettings(ctx, values, now.Unix()); err != nil {
		return nil, err
	}
	return s.statusFrom(ctx, cfg, rel), nil
}

// Apply 下载并安装最新版本，然后原地 exec 重启。
//
// 成功时本函数不会返回（进程映像被换掉）。返回错误说明升级没做成，
// 旧版本仍在运行——这是刻意的：宁可升级失败，也不要让服务停在半路。
func (s *Service) Apply(ctx context.Context) error {
	if !s.acquire() {
		return ErrBusy
	}
	// 成功路径会 exec，defer 不会执行，所以失败路径手动 release。
	cfg, err := s.LoadConfig(ctx)
	if err != nil {
		s.release()
		return err
	}
	client := s.client(cfg)
	rel := s.cachedRelease()
	if rel == nil {
		rel, err = client.Check(ctx)
		if err != nil {
			s.release()
			return err
		}
	}
	switch {
	case rel == nil:
		s.release()
		return errors.New("没有找到适合当前平台的发布")
	case !NeedsUpdate(s.opt.Current, rel.Tag):
		s.release()
		return fmt.Errorf("当前版本 %s 已不低于最新版本 %s", s.opt.Current, rel.Tag)
	case s.self == "":
		s.release()
		return errors.New("无法定位当前可执行文件，不能自动升级")
	}

	sums, err := client.FetchChecksums(ctx, rel.ChecksumURL)
	if err != nil {
		s.release()
		s.fail(ctx, err)
		return err
	}
	want := sums[rel.AssetName]
	if want == "" {
		err = fmt.Errorf("checksums.txt 里没有 %s 的摘要", rel.AssetName)
		s.release()
		s.fail(ctx, err)
		return err
	}

	// 直接下载到目标目录的临时文件：校验通过后 rename 就能替换，
	// 不必再复制一遍 14MB；失败时删掉临时文件，目录里干干净净。
	tmp, err := os.CreateTemp(filepath.Dir(s.self), ".bridge-download-*")
	if err != nil {
		s.release()
		err = fmt.Errorf("%w: %v", ErrNotWritable, err)
		s.fail(ctx, err)
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	if _, err := client.Download(ctx, rel.AssetURL, tmp); err != nil {
		s.release()
		s.fail(ctx, err)
		return err
	}
	if err := tmp.Sync(); err != nil {
		s.release()
		s.fail(ctx, err)
		return err
	}
	if err := tmp.Close(); err != nil {
		s.release()
		s.fail(ctx, err)
		return err
	}

	now := s.now().Unix()
	// 记下"正在升级"：万一 exec 之后进程起不来，下次启动能看到是升级导致的。
	_ = s.st.SetSettings(ctx, map[string]string{
		KeyPendingFrom: s.opt.Current,
		KeyPendingTo:   rel.Tag,
		KeyLastRun:     strconv.FormatInt(now, 10),
	}, now)

	backup, installErr := InstallFile(tmpName, s.self, want)
	if installErr != nil {
		s.release()
		s.fail(ctx, installErr)
		return installErr
	}

	log.Printf("升级包已就位：%s -> %s（旧版本备份在 %s），准备重启", s.opt.Current, rel.Tag, backup)
	_ = s.st.SetSettings(ctx, map[string]string{
		KeyLastResult:  ResultOK,
		KeyLastMessage: "已升级到 " + rel.Tag + "，正在重启",
	}, now)

	if s.onBeforeRestart != nil {
		s.onBeforeRestart()
	}
	// 这里之后本进程映像会被替换；成功即不再返回。
	return s.restart()
}

// Rollback 用升级时留下的 .bak 恢复上一个版本。
//
// 只替换文件，不重启：回滚这个动作本身不该打断正在处理的消息。
// 用户重启（或服务管理器重启）后生效。
func (s *Service) Rollback() error {
	if s.self == "" {
		return errors.New("无法定位当前可执行文件，不能回滚")
	}
	backup := s.self + ".bak"
	src, err := os.Open(backup)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("没有找到备份文件，无法回滚（可能从未升级过）")
		}
		return fmt.Errorf("打开备份失败: %w", err)
	}
	defer src.Close()

	if _, err := Install(src, s.self, ""); err != nil {
		return err
	}
	now := s.now().Unix()
	_ = s.st.SetSettings(ctx0(), map[string]string{
		KeyLastResult:  ResultOK,
		KeyLastMessage: "已回滚到上一个版本，重启后生效",
	}, now)
	log.Printf("已用 %s 回滚，重启后生效", backup)
	return nil
}

// ctx0 给没有请求上下文的内部调用一个兜底 context。
func ctx0() context.Context { return context.Background() }

// fail 记录一次失败。
func (s *Service) fail(ctx context.Context, cause error) {
	now := s.now().Unix()
	msg := cause.Error()
	_ = s.st.SetSettings(ctx, map[string]string{
		KeyLastResult:  ResultError,
		KeyLastMessage: "升级失败：" + msg,
		KeyLastError:   msg,
		KeyLastRun:     strconv.FormatInt(now, 10),
	}, now)
	log.Printf("在线升级失败: %v", cause)
}

func (s *Service) client(cfg Config) *Client {
	opt := s.opt
	opt.Channel = cfg.Channel
	return NewClient(opt)
}

func (s *Service) cachedRelease() *Release {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest
}

func (s *Service) cachedCheckAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCheckAt
}

// Status 返回页面快照。
func (s *Service) Status(ctx context.Context) (*Status, error) {
	cfg, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	return s.statusFrom(ctx, cfg, s.cachedRelease()), nil
}

func (s *Service) statusFrom(ctx context.Context, cfg Config, rel *Release) *Status {
	st := &Status{
		Current:    s.opt.Current,
		Config:     cfg,
		Latest:     rel,
		SelfPath:   s.self,
		AssetName:  AssetName(s.opt.GOOS, s.opt.GOARCH),
		Platform:   s.opt.GOOS + "/" + s.opt.GOARCH,
		Running:    s.isRunning(),
		CanUpgrade: s.opt.GOOS != "windows" && s.self != "",
	}
	if rel != nil {
		st.HasUpdate = NeedsUpdate(s.opt.Current, rel.Tag)
	}

	values := map[string]string{}
	for _, k := range []string{KeyLastCheck, KeyLastResult, KeyLastMessage, KeyLastRun,
		KeyLastError, KeyPendingFrom, KeyPendingTo, KeyLastFrom, KeyLastTo} {
		if v, err := s.st.GetSetting(ctx, k); err == nil {
			values[k] = v
		}
	}
	st.LastCheck, _ = strconv.ParseInt(values[KeyLastCheck], 10, 64)
	st.LastRun, _ = strconv.ParseInt(values[KeyLastRun], 10, 64)
	st.LastResult = values[KeyLastResult]
	st.LastMessage = values[KeyLastMessage]
	st.LastError = values[KeyLastError]
	if st.LastResult == "" {
		st.LastResult = ResultNone
	}
	// "刚完成升级"的提示：标记还在说明还没确认（进程没重启），
	// 或者已确认但确认时间很近——两种情况都值得在页面顶部说一句。
	from, to := values[KeyPendingFrom], values[KeyPendingTo]
	if from == "" || to != s.opt.Current {
		from, to = values[KeyLastFrom], values[KeyLastTo]
	}
	if from != "" && to == s.opt.Current && (st.LastRun == 0 || s.now().Unix()-st.LastRun < 1800) {
		st.JustUpgrade = &JustDone{From: from, To: to}
	}
	return st
}

// ConfirmStartup 在启动时调用：确认上一次升级是否成功，并清掉标记。
func (s *Service) ConfirmStartup(ctx context.Context) {
	from, err := s.st.GetSetting(ctx, KeyPendingFrom)
	if err != nil || from == "" {
		return
	}
	to, _ := s.st.GetSetting(ctx, KeyPendingTo)
	now := s.now().Unix()
	if to != "" && to == s.opt.Current {
		log.Printf("在线升级完成：%s -> %s", from, to)
		_ = s.st.SetSettings(ctx, map[string]string{
			KeyLastResult:  ResultOK,
			KeyLastMessage: "已从 " + from + " 升级到 " + to,
			KeyLastFrom:    from,
			KeyLastTo:      to,
			KeyLastError:   "",
		}, now)
	} else {
		// 标记还在但版本没变：exec 失败或进程被旧版本拉起，提示可回滚。
		log.Printf("上次升级未生效（期望 %s，实际 %s），旧版本备份仍在", to, s.opt.Current)
		_ = s.st.SetSettings(ctx, map[string]string{
			KeyLastResult:  ResultError,
			KeyLastMessage: fmt.Sprintf("升级到 %s 未生效，当前仍是 %s", to, s.opt.Current),
			KeyLastError:   "升级后版本未变化",
		}, now)
	}
	_ = s.st.DeleteSetting(ctx, KeyPendingFrom)
	_ = s.st.DeleteSetting(ctx, KeyPendingTo)
}

// Run 启动后台定时器：每天在配置的时刻检查并升级。
//
// 每 30 秒对一次表，用"今天这个时刻是否已经跑过"判断该不该触发，
// 而不是用 time.Timer 算到下一个时刻——后者在用户改配置、系统时间跳变、
// 进程刚起来时都要额外处理，对一次低频任务不值得。
func (s *Service) Run(ctx context.Context) {
	if !IsComparable(s.opt.Current) {
		log.Printf("当前版本 %q 无法与发布号比较，跳过启动检查与定时升级（手动升级仍可用）", s.opt.Current)
		return
	}
	if cfg, err := s.LoadConfig(ctx); err == nil && cfg.StartupCheck {
		go func() {
			if _, err := s.Check(ctx, true); err != nil {
				log.Printf("启动检查更新失败: %v", err)
			}
		}()
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg, err := s.LoadConfig(ctx)
			if err != nil || !cfg.Auto {
				continue
			}
			now := s.now()
			if !autoDue(now, cfg.At) {
				continue
			}
			// 日期落库而不是只记在内存里：升级成功会 exec 重启，
			// 新进程的内存状态是空的，只看内存的话会在窗口内再升一次。
			day := now.Format("2006-01-02")
			if last, _ := s.st.GetSetting(ctx, KeyLastAutoDay); last == day {
				continue
			}
			_ = s.st.SetSetting(ctx, KeyLastAutoDay, day, now.Unix())
			log.Printf("定时升级触发（每天 %s）", cfg.At)
			if _, err := s.Check(ctx, true); err != nil {
				log.Printf("定时升级：检查失败: %v", err)
				continue
			}
			if err := s.Apply(ctx); err != nil {
				log.Printf("定时升级：升级失败: %v", err)
			}
		}
	}
}

// autoDue 判断此刻是否落在今天的定时窗口内。
//
// 只往前看 30 分钟，不往后无限触发：进程在下午三点启动、定时设在凌晨四点时，
// 不该立刻升级一次——那等于"装完就重启"，用户完全预料不到。
func autoDue(now time.Time, at string) bool {
	if !validClock(at) {
		return false
	}
	h, _ := strconv.Atoi(at[:2])
	m, _ := strconv.Atoi(at[3:])
	target := time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
	return !now.Before(target) && now.Before(target.Add(autoWindow))
}

func (s *Service) acquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

func (s *Service) release() {
	s.mu.Lock()
	s.running = false
	s.mu.Unlock()
}

func (s *Service) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
