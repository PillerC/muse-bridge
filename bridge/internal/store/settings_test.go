package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 缺失的键必须返回空串而不是报错：升级配置第一次读的时候一个键都没有，
// 要是报错，升级页在全新实例上就永远打不开。
func TestSettingMissingReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	v, err := st.GetSetting(ctx, SettingUpgradeChannel)
	if err != nil {
		t.Fatalf("读不存在的配置不应报错: %v", err)
	}
	if v != "" {
		t.Fatalf("不存在的配置应为空串，得到 %q", v)
	}
}

func TestSettingRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if err := st.SetSetting(ctx, SettingUpgradeAt, "04:00", 100); err != nil {
		t.Fatal(err)
	}
	v, err := st.GetSetting(ctx, SettingUpgradeAt)
	if err != nil {
		t.Fatal(err)
	}
	if v != "04:00" {
		t.Fatalf("期望 04:00，得到 %q", v)
	}

	// 再写一次是覆盖，不是插入第二行。
	if err := st.SetSetting(ctx, SettingUpgradeAt, "05:30", 200); err != nil {
		t.Fatal(err)
	}
	v, _ = st.GetSetting(ctx, SettingUpgradeAt)
	if v != "05:30" {
		t.Fatalf("覆盖后应为 05:30，得到 %q", v)
	}
}

func TestSettingsBatch(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	values := map[string]string{
		SettingUpgradeChannel: "prerelease",
		SettingUpgradeAuto:    "1",
		SettingUpgradeAt:      "03:15",
	}
	if err := st.SetSettings(ctx, values, 100); err != nil {
		t.Fatal(err)
	}

	got, err := st.GetSettings(ctx, SettingUpgradeChannel, SettingUpgradeAuto, SettingUpgradeAt, SettingUpgradeLastRun)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("期望读到 3 个键，得到 %d 个: %v", len(got), got)
	}
	if got[SettingUpgradeAt] != "03:15" {
		t.Fatalf("upgrade.at 期望 03:15，得到 %q", got[SettingUpgradeAt])
	}
	if _, ok := got[SettingUpgradeLastRun]; ok {
		t.Fatal("不存在的键不应出现在结果里")
	}
}

func TestDeleteSetting(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if err := st.SetSetting(ctx, SettingUpgradeLastError, "boom", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSetting(ctx, SettingUpgradeLastError); err != nil {
		t.Fatal(err)
	}
	v, err := st.GetSetting(ctx, SettingUpgradeLastError)
	if err != nil {
		t.Fatal(err)
	}
	if v != "" {
		t.Fatalf("删除后应为空，得到 %q", v)
	}
}
