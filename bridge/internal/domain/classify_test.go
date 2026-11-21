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

func TestClassifyPrivate(t *testing.T) {
	m := groupMsg("private", user(1, "alice", false), "hi")
	m.Chat.Type = "private"
	v := Classify(m, testBot, nil)
	if !v.ShouldReply || v.Reason != "private" {
		t.Fatalf("私聊应回复，得到 %+v", v)
	}
}

func TestClassifySelfMessage(t *testing.T) {
	m := groupMsg("private", user(900, "muse_bot", true), "echo")
	m.Chat.Type = "private"
	v := Classify(m, testBot, nil)
	if v.ShouldReply || v.Reason != "self_message" {
		t.Fatalf("自己发的消息不应回复，得到 %+v", v)
	}
}

func TestClassifyGroupNoTrigger(t *testing.T) {
	m := groupMsg("supergroup", user(1, "alice", false), "大家好")
	v := Classify(m, testBot, nil)
	if v.ShouldReply || v.Reason != "group_no_trigger" {
		t.Fatalf("群里没 @ 不应回复，得到 %+v", v)
	}
}

func TestClassifyGroupMentionEntity(t *testing.T) {
	m := groupMsg("group", user(1, "alice", false), "@muse_bot 在吗")
	m.Entities = []tg.Entity{{Type: "mention", Offset: 0, Length: 9}}
	v := Classify(m, testBot, nil)
	if !v.ShouldReply || v.Reason != "mention" {
		t.Fatalf("entity @ 应回复，得到 %+v", v)
	}
}

// v1 用 strings.Contains 判断，@muse 会命中 @musebot，导致机器人回复与他无关的消息。
func TestClassifyMentionPrefixCollision(t *testing.T) {
	m := groupMsg("group", user(1, "alice", false), "@musebot 你好")
	v := Classify(m, Bot{ID: 901, Username: "muse"}, nil)
	if v.ShouldReply {
		t.Fatalf("@musebot 不应被 @muse 命中，得到 %+v", v)
	}

	ok := groupMsg("group", user(1, "alice", false), "@muse 你好")
	if v := Classify(ok, Bot{ID: 901, Username: "muse"}, nil); !v.ShouldReply {
		t.Fatalf("@muse 应命中，得到 %+v", v)
	}

	// 结尾标点算边界
	punct := groupMsg("group", user(1, "alice", false), "@muse，你好")
	if v := Classify(punct, Bot{ID: 901, Username: "muse"}, nil); !v.ShouldReply {
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
	v := Classify(m, testBot, nil)
	if !v.ShouldReply || v.Reason != "mention" {
		t.Fatalf("带 emoji 前缀的 @ 应正确识别，得到 %+v", v)
	}
}

func TestClassifyReplyToBot(t *testing.T) {
	m := groupMsg("supergroup", user(1, "alice", false), "那这个呢")
	m.ReplyTo = &tg.Message{From: user(900, "muse_bot", true), Text: "上一句"}
	v := Classify(m, testBot, nil)
	if !v.ShouldReply || v.Reason != "reply_to_bot" {
		t.Fatalf("引用机器人应回复，得到 %+v", v)
	}
}

func TestClassifyAllowedChats(t *testing.T) {
	m := groupMsg("private", user(1, "alice", false), "hi")
	m.Chat.Type = "private"
	m.Chat.ID = 555
	v := Classify(m, testBot, []int64{1, 2})
	if v.ShouldReply || v.Reason != "chat_not_allowed" {
		t.Fatalf("白名单外的会话不应回复，得到 %+v", v)
	}
	if v := Classify(m, testBot, []int64{555}); !v.ShouldReply {
		t.Fatalf("白名单内的会话应回复，得到 %+v", v)
	}
}

func TestClassifyChannelIgnored(t *testing.T) {
	m := groupMsg("channel", user(1, "alice", false), "post")
	v := Classify(m, testBot, nil)
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
