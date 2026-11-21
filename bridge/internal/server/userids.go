package server

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var errNoUserIDs = errors.New(
	"请至少填一个 Telegram 用户 ID：在 Telegram 里私聊 @userinfobot，它回你的那串数字就是")

// parseAllowedUserIDs 把表单里的一串用户 ID 规整成 "123,456"。
//
// 手机输入法很容易打出全角逗号和中文分号，只按半角逗号切会把
// "123，456" 整个当成一个非法值，所以常见分隔符都认；顺便去重。
// 这里刻意拒绝负数和 0：那是群/频道的会话 ID，填进来会让人以为配好了，
// 实际永远匹配不到任何人。
func parseAllowedUserIDs(raw string) (string, error) {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', '，', ';', '；', ' ', '\t', '\n', '\r':
			return true
		}
		return false
	})
	seen := make(map[int64]bool, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return "", fmt.Errorf("%q 不是用户 ID：它应该是一串纯数字，例如 123456789", p)
		}
		if v <= 0 {
			return "", fmt.Errorf("%d 是群或频道的会话 ID；这里要填人的 ID（正数），可私聊 @userinfobot 获取", v)
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, strconv.FormatInt(v, 10))
	}
	if len(out) == 0 {
		return "", errNoUserIDs
	}
	if len(out) > 20 {
		return "", errors.New("最多填 20 个用户 ID")
	}
	return strings.Join(out, ","), nil
}
