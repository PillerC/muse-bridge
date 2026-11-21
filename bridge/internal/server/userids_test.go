package server

import "testing"

func TestParseAllowedUserIDs(t *testing.T) {
	cases := []struct {
		in   string
		want string
		bad  bool
	}{
		{"123", "123", false},
		{" 123 , 456 ", "123,456", false},
		// 手机输入法打出的全角逗号、中文分号都要认，否则用户会看到
		// "123，456 不是用户 ID" 这种莫名其妙的报错。
		{"123，456；789", "123,456,789", false},
		{"123 456", "123,456", false},
		{"123,123", "123", false},
		{"", "", true},
		{"   ", "", true},
		// 负数/0 是群和频道的会话 ID，填进来会永远匹配不到人。
		{"-1001234567890", "", true},
		{"0", "", true},
		{"abc", "", true},
		{"123,abc", "", true},
	}
	for _, c := range cases {
		got, err := parseAllowedUserIDs(c.in)
		if c.bad {
			if err == nil {
				t.Fatalf("parseAllowedUserIDs(%q) 应报错，得到 %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseAllowedUserIDs(%q) 报错: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("parseAllowedUserIDs(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
