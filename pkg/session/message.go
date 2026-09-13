package session

import "time"

// Message 代表一条会话中的消息。
type Message struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"` // "user" | "assistant"
	Content   string    `json:"content"`
	Files     []string  `json:"files,omitempty"` // 附件路径
	CreatedAt time.Time `json:"created_at"`
	TokenUsed int       `json:"token_used,omitempty"`
}
