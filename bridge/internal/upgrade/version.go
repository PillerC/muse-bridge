package upgrade

import (
	"strconv"
	"strings"
)

// Semver 是可比较的版本号。
//
// 只保留 major.minor.patch 用于排序，预发布/构建后缀单独记在 Pre 里：
// 按 semver 的约定，同号情况下"没有后缀"的正式版比带后缀的预发布版新。
type Semver struct {
	Major int
	Minor int
	Patch int
	Pre   string
}

// ParseSemver 解析 v1.2.3 / 1.2.3 / v1.2.3-rc1 / v1.2.3-4-gabc123 这类写法。
//
// 只接受两段或三段数字：git describe 产出的 "v1.2.3-4-gabc123" 会被解析成
// 1.2.3 加后缀 "4-gabc123"（它其实是 1.2.3 之后 4 个提交，不是预发布版，
// 所以调用方要配合 IsCleanRelease 判断能不能参与自动升级）。
func ParseSemver(s string) (Semver, bool) {
	raw := strings.TrimSpace(s)
	raw = strings.TrimPrefix(raw, "v")
	if raw == "" {
		return Semver{}, false
	}
	if i := strings.IndexByte(raw, '+'); i >= 0 {
		raw = raw[:i]
	}
	var pre string
	if i := strings.IndexByte(raw, '-'); i >= 0 {
		pre = raw[i+1:]
		raw = raw[:i]
	}
	parts := strings.Split(raw, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return Semver{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		if p == "" {
			return Semver{}, false
		}
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 {
			return Semver{}, false
		}
		nums[i] = v
	}
	return Semver{Major: nums[0], Minor: nums[1], Patch: nums[2], Pre: pre}, true
}

// Compare 比较两个版本：a 比 b 新返回 1，相同返回 0，旧返回 -1。
func Compare(a, b Semver) int {
	if c := compareInt(a.Major, b.Major); c != 0 {
		return c
	}
	if c := compareInt(a.Minor, b.Minor); c != 0 {
		return c
	}
	if c := compareInt(a.Patch, b.Patch); c != 0 {
		return c
	}
	// 同号：正式版 > 预发布版。
	switch {
	case a.Pre == "" && b.Pre == "":
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	default:
		return strings.Compare(a.Pre, b.Pre)
	}
}

func compareInt(a, b int) int {
	switch {
	case a > b:
		return 1
	case a < b:
		return -1
	default:
		return 0
	}
}

// IsCleanRelease 判断这个版本号是不是一个干净的发布号（能参与自动升级比较）。
//
// "dev"（自己 go build 出来的）和 "v1.2.3-4-gabc123"（源码比 tag 新）都不算：
// 前者无从比较，后者按 semver 会被判成"比正式版旧"，自动升级会把人降级。
func IsCleanRelease(v string) bool {
	sv, ok := ParseSemver(v)
	if !ok {
		return false
	}
	return sv.Pre == ""
}

// IsNewer 判断 candidate 是否比 current 新。
//
// current 无法解析时：只有字面量 "dev" 视为"任何正式版都比它新"，
// 其余（比如提交哈希）一律返回 false —— 宁可不升，也不要悄悄降级。
func IsNewer(current, candidate string) bool {
	cand, ok := ParseSemver(candidate)
	if !ok {
		return false
	}
	cur, ok := ParseSemver(current)
	if !ok {
		return isDev(current)
	}
	return Compare(cand, cur) > 0
}

// isDev 判断是不是没打 tag 直接 go build 出来的版本。
func isDev(v string) bool {
	return strings.TrimSpace(v) == "dev"
}

// IsDev 暴露给上层：dev 版本可以升级一次，之后就变成正式版号了。
func IsDev(v string) bool { return isDev(v) }

// IsComparable 判断版本号能否参与比较（正式发布号或 dev）。
//
// 提交哈希（git describe --always 在没有 tag 时的输出）不在其中：
// 它和发布号之间没有大小关系，自动升级无法判断"是不是已经是最新"，
// 盲目每天下载一次只是白费流量。
func IsComparable(v string) bool {
	if isDev(v) {
		return true
	}
	return IsCleanRelease(v)
}

// NeedsUpdate 判断是否该执行一次升级（供手动升级使用）。
//
// 与 IsNewer 的区别：当前版本是提交哈希这种无法比较的版本时，
// IsNewer 返回 false（保守，不降级），但手动升级是用户的明确意愿，
// 应该允许——用户自己编译的版本换成官方发布版是合理操作。
// 自动升级请用 IsComparable 把关，不要用这个。
func NeedsUpdate(current, candidate string) bool {
	if _, ok := ParseSemver(candidate); !ok {
		return false
	}
	if IsNewer(current, candidate) {
		return true
	}
	// 当前版本无法比较（既不是 dev 也不是正式号）时，允许手动升级。
	_, ok := ParseSemver(current)
	return !ok && !isDev(current)
}
