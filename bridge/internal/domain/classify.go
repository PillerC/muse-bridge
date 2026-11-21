package domain

import (
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"muse-bridge/internal/tg"
)

// Bot 是桥所服务的机器人身份。
type Bot struct {
	ID       int64
	Username string
}

// Verdict 是一条消息的过滤结论。
type Verdict struct {
	ShouldReply  bool
	IsMention    bool
	IsReplyToBot bool
	Reason       string
}

// trigger 判断一条消息在「不考虑允许列表」时是否该回复。
//
// 拆成独立函数是为了让调用方能区分两件事：
//   - 这条消息本身命中机器人了吗（私聊、@、引用）；
//   - 发消息的人被允许吗。
//
// IngestUpdate 需要前者来决定「首个发送者自动绑定」是否成立——否则在关了
// privacy mode 的群里，随便谁聊一句就会把桥认领走。
func trigger(m *tg.Message, bot Bot) Verdict {
	if m == nil {
		return Verdict{Reason: "no_message"}
	}
	chatType := m.Chat.Type

	from := m.From
	if from != nil && from.IsBot && isSelf(from, bot) {
		return Verdict{Reason: "self_message"}
	}

	text := m.Text
	if text == "" {
		text = m.Caption
	}
	entities := make([]tg.Entity, 0, len(m.Entities)+len(m.CaptionEntities))
	entities = append(entities, m.Entities...)
	entities = append(entities, m.CaptionEntities...)

	isMention := false
	for _, e := range entities {
		switch e.Type {
		case "mention":
			name := entityText(text, e)
			if strings.EqualFold(strings.TrimPrefix(name, "@"), bot.Username) {
				isMention = true
			}
		case "text_mention":
			if e.User == nil {
				continue
			}
			if bot.ID != 0 && e.User.ID == bot.ID {
				isMention = true
			}
			if bot.Username != "" && strings.EqualFold(e.User.Username, bot.Username) {
				isMention = true
			}
		}
	}
	if !isMention && bot.Username != "" && containsMention(text, bot.Username) {
		isMention = true
	}

	isReplyToBot := false
	if rt := m.ReplyTo; rt != nil && rt.From != nil && rt.From.IsBot && isSelf(rt.From, bot) {
		isReplyToBot = true
	}

	switch chatType {
	case "private":
		return Verdict{ShouldReply: true, IsMention: isMention, IsReplyToBot: isReplyToBot, Reason: "private"}
	case "group", "supergroup":
		if isMention {
			return Verdict{ShouldReply: true, IsMention: true, IsReplyToBot: isReplyToBot, Reason: "mention"}
		}
		if isReplyToBot {
			return Verdict{ShouldReply: true, IsMention: isMention, IsReplyToBot: true, Reason: "reply_to_bot"}
		}
		return Verdict{IsMention: isMention, IsReplyToBot: isReplyToBot, Reason: "group_no_trigger"}
	default:
		return Verdict{IsMention: isMention, IsReplyToBot: isReplyToBot, Reason: "unsupported_chat_" + chatType}
	}
}

// Classify 判断一条消息是否需要回答。
//
// 规则：
//   - 机器人自己发的消息永不回复（防自激循环）；没命中机器人的消息也不回；
//   - 发消息的人必须在 allowedUserIDs 里，否则不回复、也不入库；
//   - 列表为空表示拒绝（no_allowed_users）。实际运行时它很少出现：
//     空列表会在 IngestUpdate 里被「首个发送者自动绑定」接管。
//
// 白名单按「人」而不是「会话」判断：群里任何成员都可能把机器人拉进来，
// 按会话放行等于让整个群的人都能触发。
//
// 与 v1 的两处修正：
//  1. mention 用完整用户名边界匹配，`@muse` 不再误命中 `@musebot`；
//  2. entity 的 offset/length 按 Telegram 规范是 UTF-16 码元，v1 直接拿它切
//     UTF-8 字符串，遇到 emoji 会错位，这里统一转 UTF-16 再切。
func Classify(m *tg.Message, bot Bot, allowedUserIDs []int64) Verdict {
	v := trigger(m, bot)
	if !v.ShouldReply {
		// 自己发的、频道、群里没 @ 的——这些跟名单无关，先原样返回。
		return v
	}
	if len(allowedUserIDs) == 0 {
		return Verdict{IsMention: v.IsMention, IsReplyToBot: v.IsReplyToBot, Reason: "no_allowed_users"}
	}
	if m.From == nil {
		// 匿名管理员发言、频道身份发言都拿不到真实用户，无法判定，直接丢。
		return Verdict{IsMention: v.IsMention, IsReplyToBot: v.IsReplyToBot, Reason: "no_sender"}
	}
	if !containsID(allowedUserIDs, m.From.ID) {
		return Verdict{IsMention: v.IsMention, IsReplyToBot: v.IsReplyToBot, Reason: "user_not_allowed"}
	}
	return v
}

func isSelf(u *tg.User, bot Bot) bool {
	if bot.ID != 0 && u.ID == bot.ID {
		return true
	}
	return bot.Username != "" && strings.EqualFold(u.Username, bot.Username)
}

// entityText 按 UTF-16 码元截取 entity 覆盖的文本。
func entityText(text string, e tg.Entity) string {
	if e.Offset < 0 || e.Length <= 0 {
		return ""
	}
	u := utf16.Encode([]rune(text))
	if e.Offset >= len(u) {
		return ""
	}
	end := e.Offset + e.Length
	if end > len(u) {
		end = len(u)
	}
	return string(utf16.Decode(u[e.Offset:end]))
}

// containsMention 判断 text 是否真的 @ 了 username。
// 要求 @ 后面紧跟的字符序列完全等于 username，且结束处是边界，
// 避免 @muse 命中 @musebot。
func containsMention(text, username string) bool {
	if username == "" {
		return false
	}
	lower := strings.ToLower(text)
	target := "@" + strings.ToLower(username)
	for i := 0; ; {
		j := strings.Index(lower[i:], target)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(target)
		if end >= len(lower) {
			return true
		}
		// 必须按 rune 解码后判断，而不是取单个字节——
		// 中文标点占 3 个字节，取首字节会得到一个拉丁字母，误判成单词字符。
		next, _ := utf8.DecodeRuneInString(lower[end:])
		if !isWordChar(next) {
			return true
		}
		i = end
	}
}

func isWordChar(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
