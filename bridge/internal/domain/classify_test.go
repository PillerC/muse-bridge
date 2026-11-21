package domain

import (
	"testing"

	"muse-bridge/internal/tg"
)

func user(id int64, username string, isBot bool) *tg.User {
	return &tg.User{ID: id, Username: username, IsBot: isBot, FirstName: "u"}
}

func groupMsg(chatType string, from *tg.User, text string) *tg.Message {
	return &tg.Message{
		MessageID: 1,
		From:      from,
		Chat:      tg.Chat{ID: -100, Type: chatType, Title: "g"},
		Text:      text,
	}
}

var testBot = Bot{ID: 900, Username: "muse_bot"}

// alice 是唯一被允许使用这座桥的人，绝大多数用例都站在她的视角。
var aliceOnly = []int64{1}

func TestClassifyPrivate(t *testing.T) {
	m := groupMsg("private", user(1, "alice", false), "hi")
	m.Chat.Type = "private"
	v := Classify(m, testBot, aliceOnly)
	if !v.ShouldReply || v.Reason != "private" {
		t.Fatalf("私聊应回复，得到 %+v", v)
	}
}

func TestClassifySelfMessage(t *testing.T) {
	m := groupMsg("private", user(900, "muse_bot", true), "echo")
	m.Chat.Type = "private"
	v := Classify(m, testBot, aliceOnly)
	if v.ShouldReply || v.Reason != "self_message" {
		t.Fatalf("自己发的消息不应回复，得到 %+v", v)
	}
}

func TestClassifyGroupNoTrigger(t *testing.T) {
	m := groupMsg("supergroup", user(1, "alice", false), "大家好")
	v := Classify(m, testBot, aliceOnly)
	if v.ShouldReply || v.Reason != "group_no_trigger" {
		t.Fatalf("群里没 @ 不应回复，得到 %+v", v)
	}
}

func TestClassifyGroupMentionEntity(t *testing.T) {
	m := groupMsg("group", user(1, "alice", false), "@muse_bot 在吗")
	m.Entities = []tg.Entity{{Type: "mention", Offset: 0, Length: 9}}
	v := Classify(m, testBot, aliceOnly)
	if !v.ShouldReply || v.Reason != "mention" {
		t.Fatalf("entity @ 应回复，得到 %+v", v)
	}
}

// v1 用 strings.Contains 判断，@muse 会命中 @musebot，导致机器人回复与他无关的消息。
func TestClassifyMentionPrefixCollision(t *testing.T) {
	bot := Bot{ID: 901, Username: "muse"}
	m := groupMsg("group", user(1, "alice", false), "@musebot 你好")
	v := Classify(m, bot, aliceOnly)
	if v.ShouldReply {
		t.Fatalf("@musebot 不应被 @muse 命中，得到 %+v", v)
	}

	ok := groupMsg("group", user(1, "alice", false), "@muse 你好")
	if v := Classify(ok, bot, aliceOnly); !v.ShouldReply {
		t.Fatalf("@muse 应命中，得到 %+v", v)
	}

	// 结尾标点算边界
	punct := groupMsg("group", user(1, "alice", false), "@muse，你好")
	if v := Classify(punct, bot, aliceOnly); !v.ShouldReply {
		t.Fatalf("@muse 后接中文标点应命中，得到 %+v", v)
	}
}

// emoji 在 UTF-16 里占两个码元，entity offset 也是按 UTF-16 计的。
// v1 直接拿 offset 切 UTF-8 字符串，会把 entity 切错位置。
func TestClassifyMentionWithEmojiPrefix(t *testing.T) {
	text := "🎉🎉 @muse_bot hi"
	// 4 个 UTF-16 码元（两个 emoji）+ 空格 = 5
	m := groupMsg("group", user(1, "alice", false), text)
	m.Entities = []tg.Entity{{Type: "mention", Offset: 5, Length: 9}}
	v := Classify(m, testBot, aliceOnly)
	if !v.ShouldReply || v.Reason != "mention" {
		t.Fatalf("带 emoji 前缀的 @ 应正确识别，得到 %+v", v)
	}
}

func TestClassifyReplyToBot(t *testing.T) {
	m := groupMsg("supergroup", user(1, "alice", false), "那这个呢")
	m.ReplyTo = &tg.Message{From: user(900, "muse_bot", true), Text: "上一句"}
	v := Classify(m, testBot, aliceOnly)
	if !v.ShouldReply || v.Reason != "reply_to_bot" {
		t.Fatalf("引用机器人应回复，得到 %+v", v)
	}
}

// 白名单看的是发消息的人，不是会话：
// 私聊时 chat.id 也等于对方 ID，看起来一样，但群里 chat.id 是负数，
// 按人判断才能做到"同一个群里只有我能触发"。
func TestClassifyAllowedUsers(t *testing.T) {
	m := groupMsg("private", user(1, "alice", false), "hi")
	m.Chat.Type = "private"
	m.Chat.ID = 1
	if v := Classify(m, testBot, []int64{1, 2}); !v.ShouldReply {
		t.Fatalf("名单内的人私聊应回复，得到 %+v", v)
	}
	if v := Classify(m, testBot, []int64{2}); v.ShouldReply || v.Reason != "user_not_allowed" {
		t.Fatalf("名单外的人私聊不应回复，得到 %+v", v)
	}
}

// 群里陌生人 @ 机器人：以前只看会话，只要这个群在名单里就放行；
// 现在按人判断，陌生人 @ 也不回、不入库。
func TestClassifyGroupStrangerMentionIgnored(t *testing.T) {
	m := groupMsg("supergroup", user(77, "stranger", false), "@muse_bot 帮我写个脚本")
	m.Entities = []tg.Entity{{Type: "mention", Offset: 0, Length: 9}}
	v := Classify(m, testBot, aliceOnly)
	if v.ShouldReply || v.Reason != "user_not_allowed" {
		t.Fatalf("名单外的人在群里 @ 也不应回复，得到 %+v", v)
	}
}

// 空名单不再等于"不限制"。桥的另一头是 Muse，放开就会被陌生人拿去刷额度。
func TestClassifyEmptyAllowListRejectsAll(t *testing.T) {
	for _, allowed := range [][]int64{nil, []int64{}} {
		m := groupMsg("private", user(1, "alice", false), "hi")
		m.Chat.Type = "private"
		v := Classify(m, testBot, allowed)
		if v.ShouldReply || v.Reason != "no_allowed_users" {
			t.Fatalf("名单为空时应全部拒绝，得到 %+v", v)
		}
	}
}

// 匿名管理员、以频道身份发言时拿不到真实用户，一律丢弃。
func TestClassifyNoSender(t *testing.T) {
	m := groupMsg("supergroup", nil, "@muse_bot hi")
	m.Entities = []tg.Entity{{Type: "mention", Offset: 0, Length: 9}}
	v := Classify(m, testBot, aliceOnly)
	if v.ShouldReply || v.Reason != "no_sender" {
		t.Fatalf("拿不到发送者时不应回复，得到 %+v", v)
	}
}

func TestClassifyChannelIgnored(t *testing.T) {
	m := groupMsg("channel", user(1, "alice", false), "post")
	v := Classify(m, testBot, aliceOnly)
	if v.ShouldReply || v.Reason != "unsupported_chat_channel" {
		t.Fatalf("channel 不应回复，得到 %+v", v)
	}
}

func TestContentPhoto(t *testing.T) {
	m := &tg.Message{
		Caption: "看这张",
		Photo:   []tg.PhotoSize{{Width: 1280, Height: 720}},
	}
	text, kind, hint := m.Content()
	if kind != "photo" || text != "看这张" {
		t.Fatalf("图片消息解析错误: %q %q", kind, text)
	}
	if hint == "" {
		t.Fatal("图片消息必须给出可读描述，否则 agent 只会看到空文本")
	}
}

func TestNormalizeLegacyMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		// 模型最常输出的写法：双星号加粗，老式 Markdown 不认。
		{"这是 **重点** 内容", "这是 *重点* 内容"},
		{"__强调__ 和 _斜体_", "_强调_ 和 _斜体_"},
		{"***粗斜***", "*粗斜*"},
		{"没有格式的普通文字", "没有格式的普通文字"},
		{"单个 * 不动", "单个 * 不动"},
		{"代码 `a**b` 里的星号也会被压平", "代码 `a*b` 里的星号也会被压平"},
		{"多行\n**第一行**\n**第二行**", "多行\n*第一行*\n*第二行*"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeLegacyMarkdown(c.in); got != c.want {
			t.Fatalf("normalizeLegacyMarkdown(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
