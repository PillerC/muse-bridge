package upgrade

import "testing"

func TestParseSemver(t *testing.T) {
	cases := []struct {
		in   string
		want Semver
		ok   bool
	}{
		{"v1.2.3", Semver{1, 2, 3, ""}, true},
		{"1.2.3", Semver{1, 2, 3, ""}, true},
		{"v0.1", Semver{0, 1, 0, ""}, true},
		{"v2.0.0-rc1", Semver{2, 0, 0, "rc1"}, true},
		{"v1.2.3-4-gabc123", Semver{1, 2, 3, "4-gabc123"}, true},
		{"v1.2.3+build.5", Semver{1, 2, 3, ""}, true},
		{"dev", Semver{}, false},
		{"2249451", Semver{}, false},
		{"", Semver{}, false},
		{"v1.2.3.4", Semver{}, false},
		{"vabc", Semver{}, false},
	}
	for _, c := range cases {
		got, ok := ParseSemver(c.in)
		if ok != c.ok {
			t.Fatalf("ParseSemver(%q) ok=%v，期望 %v", c.in, ok, c.ok)
		}
		if ok && got != c.want {
			t.Fatalf("ParseSemver(%q)=%+v，期望 %+v", c.in, got, c.want)
		}
	}
}

func TestCompare(t *testing.T) {
	mustParse := func(s string) Semver {
		v, ok := ParseSemver(s)
		if !ok {
			t.Fatalf("解析 %q 失败", s)
		}
		return v
	}
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v1.2.4", "v1.2.3", 1},
		{"v1.3.0", "v1.2.9", 1},
		{"v2.0.0", "v1.99.99", 1},
		{"v1.2.3", "v1.2.4", -1},
		{"v0.9.0", "v1.0.0", -1},
		// 同号时正式版比预发布版新。
		{"v1.2.3", "v1.2.3-rc1", 1},
		{"v1.2.3-rc1", "v1.2.3", -1},
		{"v1.2.3-rc2", "v1.2.3-rc1", 1},
	}
	for _, c := range cases {
		if got := Compare(mustParse(c.a), mustParse(c.b)); got != c.want {
			t.Fatalf("Compare(%s,%s)=%d，期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestIsCleanRelease(t *testing.T) {
	for _, v := range []string{"v1.2.3", "1.0.0", "v0.1"} {
		if !IsCleanRelease(v) {
			t.Fatalf("%q 应视为正式发布号", v)
		}
	}
	for _, v := range []string{"dev", "2249451", "v1.2.3-4-gabc123", "v1.2.3-rc1", ""} {
		if IsCleanRelease(v) {
			t.Fatalf("%q 不应视为正式发布号", v)
		}
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
	}{
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.1", "v1.0.0", false},
		// dev 是自己编译的版本，任何正式版都比它新。
		{"dev", "v1.0.0", true},
		// 提交哈希无从比较，宁可不升也不要降级。
		{"2249451", "v1.0.0", false},
		// 候选版本号解析不出来时也不升。
		{"v1.0.0", "latest", false},
		{"v1.0.0", "", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.current, c.candidate); got != c.want {
			t.Fatalf("IsNewer(%q,%q)=%v，期望 %v", c.current, c.candidate, got, c.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	cases := []struct{ goos, goarch, want string }{
		{"linux", "amd64", "bridge-linux-amd64"},
		{"linux", "arm64", "bridge-linux-arm64"},
		{"darwin", "arm64", "bridge-darwin-arm64"},
		{"windows", "amd64", "bridge-windows-amd64.exe"},
	}
	for _, c := range cases {
		if got := AssetName(c.goos, c.goarch); got != c.want {
			t.Fatalf("AssetName(%s,%s)=%q，期望 %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func TestIsComparable(t *testing.T) {
	for _, v := range []string{"v1.2.3", "1.0.0", "dev"} {
		if !IsComparable(v) {
			t.Fatalf("%q 应可比较", v)
		}
	}
	for _, v := range []string{"2249451", "abc1234", "v1.2.3-4-gabc123", ""} {
		if IsComparable(v) {
			t.Fatalf("%q 不应可比较", v)
		}
	}
}

func TestNeedsUpdate(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
	}{
		// 正式版之间按大小比较。
		{"v1.0.0", "v1.0.1", true},
		{"v1.0.1", "v1.0.0", false},
		{"v1.0.0", "v1.0.0", false},
		// dev 可以被手动升级成任何正式版。
		{"dev", "v1.0.0", true},
		// 提交哈希无法比较：手动升级允许，自动升级由 IsComparable 拦住。
		{"2249451", "v1.0.0", true},
		// 候选版本号不合法时一律不动。
		{"v1.0.0", "latest", false},
		{"v1.0.0", "", false},
	}
	for _, c := range cases {
		if got := NeedsUpdate(c.current, c.candidate); got != c.want {
			t.Fatalf("NeedsUpdate(%q,%q)=%v，期望 %v", c.current, c.candidate, got, c.want)
		}
	}
}
