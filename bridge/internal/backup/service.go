// Package backup 的调度层：把按天归档、上传远端、清理本地这三件事串起来。
//
// 三件事的先后顺序不能变：
//
//	归档 → 上传 → 确认上传成功 → 才允许删本地
//
// 任何一步失败都停在原地，绝不跳过。尤其是删除：只有 archive_state 里
// uploaded_at>0（或用户明确选择"不备份也删"）的那一天才会被清理，
// 否则一次上传失败就永久丢数据。
package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"muse-bridge/internal/archive"
)

// Settings 是配置持久化接口，由 store 实现。
type Settings interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string, now int64) error
	SetSettings(ctx context.Context, values map[string]string, now int64) error
	DeleteSetting(ctx context.Context, key string) error
}

// Archives 是归档状态读写接口，由 store 实现。
type Archives interface {
	MarkArchived(ctx context.Context, day string, records int, bytes int64, now int64) error
	MarkUploaded(ctx context.Context, day string, now int64) error
	MarkUploadFailed(ctx context.Context, day, reason string) error
	MarkPurged(ctx context.Context, day string, now int64) error
	GetArchiveState(ctx context.Context, day string) (*ArchiveState, error)
	ListArchiveStates(ctx context.Context, limit int) ([]*ArchiveState, error)
	PendingUploads(ctx context.Context, limit int) ([]string, error)
	DaysNeedingArchive(ctx context.Context, loc *time.Location, limit int) ([]string, error)
	PurgeDay(ctx context.Context, day string, loc *time.Location) (int, error)
}

// ArchiveState 是归档状态，别名到 archive.DayState。
//
// 用别名而不是重新定义：store 实现 Archives 接口时返回的就是 archive.DayState，
// 定义两份结构体只会让两边慢慢漂移。
type ArchiveState = archive.DayState

// 配置键。
const (
	KeyType        = "backup.type"    // s3 | webdav | 空
	KeyEnabled     = "backup.enabled" // 1 | 0
	KeyPrefix      = "backup.prefix"  // 对象键前缀
	KeyS3Endpoint  = "backup.s3.endpoint"
	KeyS3Region    = "backup.s3.region"
	KeyS3Bucket    = "backup.s3.bucket"
	KeyS3PathStyle = "backup.s3.path_style"
	KeyS3AccessKey = "backup.s3.access_key"
	KeyS3SecretKey = "backup.s3.secret_key" // 加密存储
	KeyS3Token     = "backup.s3.session_token"
	KeyWebDAVURL   = "backup.webdav.url"
	KeyWebDAVUser  = "backup.webdav.user"
	KeyWebDAVPass  = "backup.webdav.password" // 加密存储

	KeyKeepDays      = "retain.keep_days"       // 本地保留天数，0 = 不清理
	KeyKeepEnabled   = "retain.enabled"         // 1 | 0
	KeyLastRun       = "backup.last_run"        // 上次执行时间
	KeyLastResult    = "backup.last_result"     // ok | error
	KeyLastMessage   = "backup.last_message"    // 结果说明
	KeyLastUploadDay = "backup.last_upload_day" // 最近成功上传的日期
)

// DefaultConfig 返回默认值：不备份、不清理。
//
// 默认关闭是刻意的：上传会把聊天记录送到第三方存储，这件事必须用户自己开。
// 保留天数默认 0（永不删除），否则装完就在后台悄悄删数据。
func DefaultConfig() Config {
	return Config{Enabled: false, Retain: false, KeepDays: 0}
}

// newTargetHook 让测试替换 New 的行为（避免真连网络）。
var newTargetHook func(Config) (Target, error)

// newTarget 构造备份目标，测试可替换。
func newTarget(cfg Config) (Target, error) {
	if newTargetHook != nil {
		return newTargetHook(cfg)
	}
	return New(cfg)
}

// Sealer 负责加解密凭据。
type Sealer interface {
	Seal(plain string) (string, error)
	Open(enc string) (string, error)
}

// Service 管理归档、上传与清理。
type Service struct {
	st  Settings
	ar  Archives
	box Sealer
	dir string // 归档目录
	loc *time.Location
	now func() time.Time
	// db 用于读取消息生成归档，由调用方注入（*sql.DB）。
	db *sql.DB

	mu      sync.Mutex
	running bool
	lastMsg string
	lastRes string
}

// NewService 构造备份服务。dir 是归档文件存放目录，db 用于读取消息。
func NewService(st Settings, ar Archives, box Sealer, db *sql.DB, dir string, loc *time.Location) *Service {
	if loc == nil {
		loc = time.Local
	}
	return &Service{st: st, ar: ar, box: box, db: db, dir: dir, loc: loc, now: time.Now}
}

// SetNowForTest 替换时钟，仅供测试使用。
func (s *Service) SetNowForTest(fn func() time.Time) { s.now = fn }

// self 返回 *sql.DB，供归档导出使用。
func (s *Service) self() *sql.DB { return s.db }

// Dir 返回归档目录。
func (s *Service) Dir() string { return s.dir }

// LoadConfig 读配置并解密凭据。
func (s *Service) LoadConfig(ctx context.Context) (Config, error) {
	cfg := DefaultConfig()
	get := func(k string) string {
		v, _ := s.st.GetSetting(ctx, k)
		return v
	}
	cfg.Enabled = get(KeyEnabled) == "1"
	cfg.Type = get(KeyType)
	cfg.Prefix = get(KeyPrefix)
	cfg.KeepDays, _ = strconv.Atoi(get(KeyKeepDays))
	cfg.Retain = get(KeyKeepEnabled) == "1"

	cfg.S3 = S3Config{
		Endpoint:  get(KeyS3Endpoint),
		Region:    get(KeyS3Region),
		Bucket:    get(KeyS3Bucket),
		PathStyle: get(KeyS3PathStyle) == "1",
		AccessKey: get(KeyS3AccessKey),
	}
	if enc := get(KeyS3SecretKey); enc != "" {
		if plain, err := s.box.Open(enc); err == nil {
			cfg.S3.SecretKey = plain
		}
	}
	if enc := get(KeyS3Token); enc != "" {
		if plain, err := s.box.Open(enc); err == nil {
			cfg.S3.SessionToken = plain
		}
	}

	cfg.WebDAV = WebDAVConfig{
		URL:      get(KeyWebDAVURL),
		Username: get(KeyWebDAVUser),
	}
	if enc := get(KeyWebDAVPass); enc != "" {
		if plain, err := s.box.Open(enc); err == nil {
			cfg.WebDAV.Password = plain
		}
	}
	return cfg, nil
}

// SaveConfig 校验并保存配置。凭据加密后落库。
func (s *Service) SaveConfig(ctx context.Context, cfg Config) error {
	if cfg.KeepDays < 0 || cfg.KeepDays > 3650 {
		return errors.New("保留天数应在 0–3650 之间（0 表示不清理）")
	}
	// 只有启用了备份才校验目标配置：用户可以先只开清理。
	if cfg.Enabled {
		if err := (Config{
			Type: cfg.Type, S3: cfg.S3, WebDAV: cfg.WebDAV, Prefix: cfg.Prefix,
		}).Validate(); err != nil {
			return err
		}
	}
	if cfg.Retain && cfg.KeepDays == 0 {
		return errors.New("启用了自动清理就必须设置保留天数")
	}
	// 删本地记录有丢数据的风险，必须确认备份真的能工作。
	if cfg.Retain && !cfg.Enabled {
		return errors.New("自动清理需要先启用在线备份：没有备份就删本地等于丢数据")
	}

	values := map[string]string{
		KeyType:        cfg.Type,
		KeyEnabled:     boolStr(cfg.Enabled),
		KeyPrefix:      cfg.Prefix,
		KeyS3Endpoint:  cfg.S3.Endpoint,
		KeyS3Region:    cfg.S3.Region,
		KeyS3Bucket:    cfg.S3.Bucket,
		KeyS3PathStyle: boolStr(cfg.S3.PathStyle),
		KeyS3AccessKey: cfg.S3.AccessKey,
		KeyWebDAVURL:   cfg.WebDAV.URL,
		KeyWebDAVUser:  cfg.WebDAV.Username,
		KeyKeepDays:    strconv.Itoa(cfg.KeepDays),
		KeyKeepEnabled: boolStr(cfg.Retain),
	}
	if cfg.S3.SecretKey != "" {
		enc, err := s.box.Seal(cfg.S3.SecretKey)
		if err != nil {
			return fmt.Errorf("加密 S3 Secret Key 失败: %w", err)
		}
		values[KeyS3SecretKey] = enc
	}
	if cfg.S3.SessionToken != "" {
		enc, err := s.box.Seal(cfg.S3.SessionToken)
		if err != nil {
			return fmt.Errorf("加密 Session Token 失败: %w", err)
		}
		values[KeyS3Token] = enc
	}
	if cfg.WebDAV.Password != "" {
		enc, err := s.box.Seal(cfg.WebDAV.Password)
		if err != nil {
			return fmt.Errorf("加密 WebDAV 密码失败: %w", err)
		}
		values[KeyWebDAVPass] = enc
	}
	return s.st.SetSettings(ctx, values, s.now().Unix())
}

// TestConnection 用当前（或传入的）配置验证能否连上远端。
//
// 传入的 cfg 优先：用户改了表单还没保存就想先测一下，
// 这时候不该逼他先保存一份可能是错的配置。
func (s *Service) TestConnection(ctx context.Context, cfg Config) error {
	target, err := newTarget(cfg)
	if err != nil {
		return err
	}
	return target.Ping(ctx)
}

// Status 是给后台页面的快照。
type Status struct {
	Config      Config
	Dir         string
	Days        []*DayRow
	KeepDays    int
	LastRun     int64
	LastResult  string
	LastMessage string
	Running     bool
	// LocalBytes 是归档目录占用的空间。
	LocalBytes int64
	// LocalCount 是归档文件数。
	LocalCount int
}

// DayRow 是页面上一行"某天"。
type DayRow struct {
	Day         string
	Records     int
	Bytes       int64
	ArchivedAt  int64
	UploadedAt  int64
	UploadError string
	PurgedAt    int64
	// FileExists 表示本地归档文件还在。
	FileExists bool
	// HasMessages 表示库里还有这一天的消息。
	HasMessages bool
}

// Status 汇总当前状态。
func (s *Service) Status(ctx context.Context) (*Status, error) {
	cfg, err := s.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	files, err := archive.List(s.dir)
	if err != nil {
		return nil, err
	}
	fileByDay := map[string]*archive.DayFile{}
	var total int64
	for _, f := range files {
		fileByDay[f.Day] = f
		total += f.Bytes
	}

	states, err := s.ar.ListArchiveStates(ctx, 120)
	if err != nil {
		return nil, err
	}
	hasMsg, _ := s.ar.DaysNeedingArchive(ctx, s.loc, 1000)
	needArchive := map[string]bool{}
	for _, d := range hasMsg {
		needArchive[d] = true
	}

	st := &Status{
		Config: cfg, Dir: s.dir, KeepDays: cfg.KeepDays,
		LocalBytes: total, LocalCount: len(files), Running: s.isRunning(),
	}
	seen := map[string]bool{}
	for _, a := range states {
		seen[a.Day] = true
		row := &DayRow{
			Day: a.Day, Records: a.Records, Bytes: a.Bytes,
			ArchivedAt: a.ArchivedAt, UploadedAt: a.UploadedAt,
			UploadError: a.UploadError, PurgedAt: a.PurgedAt,
			FileExists: fileByDay[a.Day] != nil, HasMessages: needArchive[a.Day],
		}
		if f := fileByDay[a.Day]; f != nil && row.Bytes == 0 {
			row.Bytes = f.Bytes
		}
		st.Days = append(st.Days, row)
	}
	// 有消息但还没归档的日子也要列出来，否则用户看不到"还差哪天"。
	for d := range needArchive {
		if seen[d] {
			continue
		}
		st.Days = append(st.Days, &DayRow{Day: d, HasMessages: true,
			FileExists: fileByDay[d] != nil})
	}
	sortDays(st.Days)

	st.LastRun, _ = strconv.ParseInt(s.getSetting(ctx, KeyLastRun), 10, 64)
	st.LastResult = s.getSetting(ctx, KeyLastResult)
	st.LastMessage = s.getSetting(ctx, KeyLastMessage)
	if st.LastResult == "" {
		st.LastResult = "none"
	}
	return st, nil
}

func (s *Service) getSetting(ctx context.Context, key string) string {
	v, _ := s.st.GetSetting(ctx, key)
	return v
}

func sortDays(rows []*DayRow) {
	// 日期字符串 YYYY-MM-DD 的字典序就是时间序，倒序排即可。
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].Day > rows[j-1].Day; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// RunOnce 执行一轮完整的"归档 → 上传 → 清理"。
//
// 返回错误只表示"这一轮有问题"，各步骤内部已经尽力完成能做的部分：
// 某天上传失败不会阻止其他天继续上传，也不会阻止已上传的那些被清理。
func (s *Service) RunOnce(ctx context.Context) error {
	if !s.acquire() {
		return errors.New("已有备份任务在执行")
	}
	defer s.release()

	cfg, err := s.LoadConfig(ctx)
	if err != nil {
		s.record(ctx, "error", "读取配置失败："+err.Error())
		return err
	}
	now := s.now()
	var problems []string

	// 1. 归档：把还没归档的日子生成文件（含今天）。
	days, err := s.ar.DaysNeedingArchive(ctx, s.loc, 400)
	if err != nil {
		problems = append(problems, "查询待归档日期失败："+err.Error())
	}
	for _, day := range days {
		if err := s.archiveDay(ctx, day); err != nil {
			problems = append(problems, day+" 归档失败："+err.Error())
		}
	}

	// 2. 上传：把所有已归档但未上传的日子传上去。
	if cfg.Enabled {
		problems = append(problems, s.uploadPending(ctx, cfg)...)
	}

	// 3. 清理：只删"已上传"且早于保留期的日子。
	if cfg.Retain && cfg.KeepDays > 0 {
		if err := s.purge(ctx, cfg, now); err != nil {
			problems = append(problems, "清理本地记录失败："+err.Error())
		}
	}

	msg := "完成"
	res := "ok"
	if len(problems) > 0 {
		res = "error"
		msg = joinProblems(problems)
	}
	s.record(ctx, res, msg)
	return nil
}

func joinProblems(list []string) string {
	out := ""
	for i, p := range list {
		if i > 0 {
			out += "；"
		}
		out += p
	}
	if len(out) > 500 {
		out = out[:500] + "…"
	}
	return out
}

// archiveDay 归档一天。
func (s *Service) archiveDay(ctx context.Context, day string) error {
	file, err := archive.ExportDay(ctx, s.db, s.dir, day, s.loc)
	if err != nil {
		return err
	}
	// 空文件（那天其实没有消息）删掉，别在目录里堆一堆 0 字节。
	if file.Records == 0 {
		os.Remove(file.Path)
		return s.ar.MarkArchived(ctx, day, 0, 0, s.now().Unix())
	}
	log.Printf("归档 %s：%d 条，%d 字节", day, file.Records, file.Bytes)
	return s.ar.MarkArchived(ctx, day, file.Records, file.Bytes, s.now().Unix())
}

// uploadPending 上传所有待上传的归档，返回遇到的问题。
//
// 必须把失败原因返回给调用方：早期版本在这里只 log 不返回，
// 结果 WebDAV 完全不可达时页面照样显示"执行完成"，用户以为备份好了。
// 备份失败而用户不知道，比备份功能本身坏掉更危险。
func (s *Service) uploadPending(ctx context.Context, cfg Config) []string {
	pending, err := s.ar.PendingUploads(ctx, 400)
	if err != nil {
		return []string{"查询待上传日期失败：" + err.Error()}
	}
	if len(pending) == 0 {
		return nil
	}
	target, err := newTarget(cfg)
	if err != nil {
		return []string{"备份目标配置无效：" + err.Error()}
	}

	var problems []string
	failed := 0
	for _, day := range pending {
		path := filepath.Join(s.dir, archive.FileName(day))
		if _, err := os.Stat(path); err != nil {
			// 文件不在了（被手动删了）：重新归档一次。
			if err := s.archiveDay(ctx, day); err != nil {
				_ = s.ar.MarkUploadFailed(ctx, day, "本地归档文件缺失且重新归档失败: "+err.Error())
				failed++
				continue
			}
		}
		key := joinKey(cfg.Prefix, archive.FileName(day))
		if err := target.Put(ctx, key, path); err != nil {
			_ = s.ar.MarkUploadFailed(ctx, day, err.Error())
			log.Printf("上传 %s 失败: %v", day, err)
			failed++
			continue
		}
		if err := s.ar.MarkUploaded(ctx, day, s.now().Unix()); err != nil {
			log.Printf("记录 %s 上传状态失败: %v", day, err)
		}
		_ = s.st.SetSetting(ctx, KeyLastUploadDay, day, s.now().Unix())
		log.Printf("已上传 %s -> %s", day, key)
	}
	if failed > 0 {
		// 只报"几天失败 + 第一个原因"：同样的原因重复 400 遍对用户没意义。
		problems = append(problems, fmt.Sprintf("%d 天上传失败（%s）", failed, firstUploadError(ctx, s, pending)))
	}
	return problems
}

// firstUploadError 取第一个失败日期的错误原因，用于汇总提示。
func firstUploadError(ctx context.Context, s *Service, days []string) string {
	for _, d := range days {
		if st, err := s.ar.GetArchiveState(ctx, d); err == nil && st != nil && st.UploadError != "" {
			return st.UploadError
		}
	}
	return "原因未知"
}

// purge 删除早于保留期的本地消息。
//
// 两个前置条件缺一不可：启用清理、且这一天确实上传成功过。
// 第二条是这个功能的安全底线——上传失败还删本地，就是永久丢数据。
func (s *Service) purge(ctx context.Context, cfg Config, now time.Time) error {
	cutoff := archive.CutoffDay(now, cfg.KeepDays)
	if cutoff == "" {
		return nil
	}
	states, err := s.ar.ListArchiveStates(ctx, 2000)
	if err != nil {
		return err
	}
	for _, a := range states {
		if a.Day >= cutoff || a.PurgedAt > 0 {
			continue
		}
		if a.UploadedAt == 0 {
			// 没上传成功的绝不删。
			continue
		}
		n, err := s.ar.PurgeDay(ctx, a.Day, s.loc)
		if err != nil {
			return fmt.Errorf("%s 删除失败: %w", a.Day, err)
		}
		if err := s.ar.MarkPurged(ctx, a.Day, now.Unix()); err != nil {
			return err
		}
		log.Printf("已清理本地记录 %s（%d 条，保留 %d 天）", a.Day, n, cfg.KeepDays)
	}
	return nil
}

// Backfill 手动触发：归档所有还没归档的日子并立即上传。
func (s *Service) Backfill(ctx context.Context) error { return s.RunOnce(ctx) }

// UploadDay 手动上传某一天（补传）。
func (s *Service) UploadDay(ctx context.Context, day string) error {
	cfg, err := s.LoadConfig(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return errors.New("还没有启用在线备份")
	}
	if err := s.archiveDay(ctx, day); err != nil {
		return err
	}
	path := filepath.Join(s.dir, archive.FileName(day))
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s 没有可上传的归档文件（那天可能没有消息）", day)
	}
	target, err := newTarget(cfg)
	if err != nil {
		return err
	}
	key := joinKey(cfg.Prefix, archive.FileName(day))
	if err := target.Put(ctx, key, path); err != nil {
		_ = s.ar.MarkUploadFailed(ctx, day, err.Error())
		return err
	}
	return s.ar.MarkUploaded(ctx, day, s.now().Unix())
}

func (s *Service) record(ctx context.Context, result, msg string) {
	now := s.now().Unix()
	s.mu.Lock()
	s.lastRes, s.lastMsg = result, msg
	s.mu.Unlock()
	_ = s.st.SetSettings(ctx, map[string]string{
		KeyLastRun:     strconv.FormatInt(now, 10),
		KeyLastResult:  result,
		KeyLastMessage: msg,
	}, now)
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

func boolStr(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// RunDaily 启动每日归档任务。
//
// 每小时检查一次"今天归档了吗"，而不是用定时器算到明天零点：
// 进程可能随时重启（升级会 exec），算好的定时器会丢；
// 用"今天有没有跑过"这个可持久化的判据最稳。
func RunDaily(ctx context.Context, s *Service) {
	// 启动时先补一轮：关机期间攒下的日子要补归档。
	if err := s.RunOnce(ctx); err != nil {
		log.Printf("启动归档失败: %v", err)
	}

	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.RunOnce(ctx); err != nil {
				log.Printf("定时归档失败: %v", err)
			}
		}
	}
}
