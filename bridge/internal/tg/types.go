package tg

import "strconv"

// Update 是 Telegram getUpdates / webhook 的最小载荷。
// 只保留桥会用到的字段，其余丢弃以节省带宽和内存。
type Update struct {
	UpdateID      int64    `json:"update_id"`
	Message       *Message `json:"message"`
	EditedMessage *Message `json:"edited_message"`
}

type Message struct {
	MessageID       int64       `json:"message_id"`
	From            *User       `json:"from"`
	Chat            Chat        `json:"chat"`
	Date            int64       `json:"date"`
	Text            string      `json:"text"`
	Caption         string      `json:"caption"`
	Entities        []Entity    `json:"entities"`
	CaptionEntities []Entity    `json:"caption_entities"`
	ReplyTo         *Message    `json:"reply_to_message"`
	Photo           []PhotoSize `json:"photo"`
	Voice           *Voice      `json:"voice"`
	Audio           *Audio      `json:"audio"`
	Video           *Video      `json:"video"`
	Document        *Document   `json:"document"`
	Sticker         *Sticker    `json:"sticker"`
}

type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// DisplayName 返回 "名 姓" 形式的展示名。
func (u *User) DisplayName() string {
	if u == nil {
		return ""
	}
	if u.FirstName == "" && u.LastName == "" {
		return u.Username
	}
	if u.LastName == "" {
		return u.FirstName
	}
	return u.FirstName + " " + u.LastName
}

type Chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	User   *User  `json:"user"`
}

type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

type Voice struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

type Audio struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
	Title    string `json:"title"`
}

type Video struct {
	FileID   string `json:"file_id"`
	Duration int    `json:"duration"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
}

type Sticker struct {
	FileID     string `json:"file_id"`
	Emoji      string `json:"emoji"`
	IsAnimated bool   `json:"is_animated"`
}

// Content 把消息归一成 "文本 + 类型 + 给模型看的可读描述"。
// 纯图片/语音这类没有文字的，描述里带占位说明，避免 agent 拿到空字符串发呆。
func (m *Message) Content() (text, kind, hint string) {
	text = m.Text
	kind = "text"
	if text == "" {
		text = m.Caption
	}
	if text != "" && m.Caption != "" {
		kind = "caption"
	}

	switch {
	case len(m.Photo) > 0:
		kind = "photo"
		w, h := 0, 0
		if n := len(m.Photo); n > 0 {
			w, h = m.Photo[n-1].Width, m.Photo[n-1].Height
		}
		hint = "[图片] " + strconv.Itoa(w) + "x" + strconv.Itoa(h)
		if text != "" {
			hint += "，配文：" + text
		}
		return text, kind, hint
	case m.Voice != nil:
		return text, "voice", "[语音] " + strconv.Itoa(m.Voice.Duration) + " 秒"
	case m.Audio != nil:
		return text, "audio", "[音频] " + m.Audio.Title
	case m.Video != nil:
		return text, "video", "[视频] " + strconv.Itoa(m.Video.Duration) + " 秒"
	case m.Document != nil:
		return text, "document", "[文件] " + m.Document.FileName
	case m.Sticker != nil:
		return text, "sticker", "[贴纸] " + m.Sticker.Emoji
	}
	hint = text
	return text, kind, hint
}
