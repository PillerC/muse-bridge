package store

import (
	"context"
	"path/filepath"
	"testing"

	"muse-bridge/internal/secret"
)

// 后台能改密码之后，环境变量只能当"首次创建"用：
// 每次启动都覆盖的话，网页上刚改的密码下次重启就没了。
func TestEnsureAdminKeepsExistingPassword(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.EnsureAdmin(ctx, "goojia", secret.HashPassword("first"), 1); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureAdmin(ctx, "goojia", secret.HashPassword("second"), 2); err != nil {
		t.Fatal(err)
	}

	a, err := st.GetAdmin(ctx, "goojia")
	if err != nil {
		t.Fatal(err)
	}
	if !secret.VerifyPassword(a.PasswordHash, "first") {
		t.Fatal("已存在时不该被 env 里的新密码覆盖")
	}
	if secret.VerifyPassword(a.PasswordHash, "second") {
		t.Fatal("新密码不该生效")
	}

	all, err := st.ListAdmins(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("EnsureAdmin 不该重复建账号，实际 %d 行", len(all))
	}

	// 显式重置（BRIDGE_ADMIN_RESET=1 走的是 UpsertAdmin）仍然要能覆盖。
	if err := st.UpsertAdmin(ctx, "goojia", secret.HashPassword("second"), 3); err != nil {
		t.Fatal(err)
	}
	a, _ = st.GetAdmin(ctx, "goojia")
	if !secret.VerifyPassword(a.PasswordHash, "second") {
		t.Fatal("UpsertAdmin 应强制覆盖密码")
	}
}

func TestUpdateAdminRenamesAndRehashes(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.EnsureAdmin(ctx, "admin", secret.HashPassword("pw"), 1); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetAdmin(ctx, "admin")
	if err := st.UpdateAdmin(ctx, a.ID, "goojia", secret.HashPassword("newpw")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAdmin(ctx, "admin"); err == nil {
		t.Fatal("改名后旧用户名不该还能查到")
	}
	b, err := st.GetAdmin(ctx, "goojia")
	if err != nil {
		t.Fatal(err)
	}
	if !secret.VerifyPassword(b.PasswordHash, "newpw") {
		t.Fatal("密码摘要没有更新")
	}
}
