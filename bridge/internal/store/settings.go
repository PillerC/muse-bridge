package store

import (
	"context"
	"database/sql"
	"errors"
)

// settings 是全局键值配置的存储。
//
// 升级配置（通道、定时开关、定时时刻）和最近一次升级结果都放这里。
// 没有单独建升级表：这些值天然是"一个实例一份"，键值表足够，
// 以后加别的开关也不用再动 schema。

// Setting 键名。集中定义，避免字符串散落在 handler 和 service 里。
const (
	SettingUpgradeChannel      = "upgrade.channel"       // release | prerelease
	SettingUpgradeAuto         = "upgrade.auto"          // 1 | 0
	SettingUpgradeAt           = "upgrade.at"            // HH:MM
	SettingUpgradeLastCheck    = "upgrade.last_check"    // unix 秒
	SettingUpgradeLastVersion  = "upgrade.last_version"  // 最近一次看到的新版本号
	SettingUpgradeLastResult   = "upgrade.last_result"   // ok | error | none
	SettingUpgradeLastMessage  = "upgrade.last_message"  // 结果说明
	SettingUpgradeLastRun      = "upgrade.last_run"      // 最近一次升级动作的时间
	SettingUpgradeLastError    = "upgrade.last_error"    // 失败原因
	SettingUpgradePendingFrom  = "upgrade.pending_from"  // 升级前版本，用于启动后确认
	SettingUpgradePendingTo    = "upgrade.pending_to"    // 期望升级到的版本
	SettingUpgradeStartupCheck = "upgrade.startup_check" // 1 | 0：启动时自动检查
	SettingUpgradeLastFrom     = "upgrade.last_from"     // 最近一次成功升级的来源版本
	SettingUpgradeLastTo       = "upgrade.last_to"       // 最近一次成功升级的目标版本
	SettingUpgradeLastAutoDay  = "upgrade.last_auto_day" // 定时升级最近执行的日期
)

// GetSetting 读一个配置，不存在时返回空字符串（不报错）。
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return v, nil
}

// SetSetting 写一个配置。
func (s *Store) SetSetting(ctx context.Context, key, value string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, now)
	return err
}

// DeleteSetting 删一个配置。
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, key)
	return err
}

// GetSettings 一次读多个键，返回 键 -> 值；缺失的键不会出现在结果里。
//
// 后台升级页要读七八个键，逐个查要走七八次 SQLite 往返，一次 IN 查询更省。
func (s *Store) GetSettings(ctx context.Context, keys ...string) (map[string]string, error) {
	out := map[string]string{}
	if len(keys) == 0 {
		return out, nil
	}
	query := `SELECT key, value FROM settings WHERE key IN (?` + placeholders(len(keys)-1) + `)`
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSettings 在一个事务里写多个配置。
func (s *Store) SetSettings(ctx context.Context, values map[string]string, now int64) error {
	if len(values) == 0 {
		return nil
	}
	return s.Tx(ctx, func(tx *sql.Tx) error {
		for k, v := range values {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
				 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
				k, v, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// placeholders 返回 ",?" 重复 n 次，用于 IN 子句。
func placeholders(n int) string {
	out := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		out = append(out, ',', '?')
	}
	return string(out)
}
