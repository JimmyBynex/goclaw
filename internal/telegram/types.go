package telegram

type Update struct {
	UpdateId int      `json:"update_id"`
	Message  *Message `json:"message"` //不一定包含Message，可能是其他事件
}

type Message struct {
	MessageID int    `json:"message_id"`
	Chat      Chat   `json:"chat"` //是发在private，group，channel这个地方
	From      *User  `json:"from"` //具体的发送者
	Text      string `json:"text"`
}
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type User struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type getUpdatesResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

type sendMessageResponse struct {
	OK     bool    `json:"ok"`
	Result Message `json:"result"`
}
