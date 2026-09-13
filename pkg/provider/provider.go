// Package provider 定义了所有 AI 平台必须实现的通用接口。
package provider

import (
	"context"
	"time"
)

// FileAttachment 代表一个上传的文件附件。
type FileAttachment struct {
	Path     string `json:"path"`
	MIMEType string `json:"mime_type"` // 由系统自动检测
	Size     int64  `json:"size"`
}

// SendRequest 封装发送消息所需的参数。
type SendRequest struct {
	Prompt string           `json:"prompt"`
	Files  []FileAttachment `json:"files,omitempty"`
	// SessionID 是本地会话 ID（otter 自己的 sess_* 标识），仅用于本地记账。
	SessionID string `json:"session_id,omitempty"`
	// ChatSessionID 是平台侧的会话标识（如 DeepSeek 的 UUID）。
	ChatSessionID string `json:"chat_session_id,omitempty"`
	// ParentMessageID 是多轮对话的上一条回复 ID；0 表示新会话首轮。
	ParentMessageID int `json:"parent_message_id,omitempty"`
	// ParentMessageIDStr 是字符串形式的上一条回复 ID（ChatGPT/Gemini 等平台使用）。
	ParentMessageIDStr string `json:"parent_message_id_str,omitempty"`
	// RemoteMetadata 承载平台特定的续聊元数据（如 Gemini 的 conversation metadata）。
	RemoteMetadata map[string]string `json:"remote_metadata,omitempty"`
	Timeout        time.Duration     `json:"timeout,omitempty"`
}

// SendResponse 包含发送消息后的回复结果。
type SendResponse struct {
	SessionID string        `json:"session_id"`
	Content   string        `json:"content"`
	RawHTML   string        `json:"raw_html,omitempty"` // 原始 HTML，便于不同格式输出
	TokenUsed int           `json:"token_used,omitempty"`
	Duration  time.Duration `json:"duration"`
	// ResponseMessageID 是本轮回复 ID，下次追问需作为 ParentMessageID。
	ResponseMessageID int `json:"response_message_id,omitempty"`
	// ResponseMessageIDStr 是字符串形式的回复 ID（ChatGPT/Gemini 等平台使用）。
	ResponseMessageIDStr string `json:"response_message_id_str,omitempty"`
	// RemoteConversationID 是平台侧会话 ID（ChatGPT conversation_id / Gemini cid）。
	RemoteConversationID string `json:"remote_conversation_id,omitempty"`
	// RemoteMetadata 是本轮响应的平台特定元数据，供下一轮续聊透传。
	RemoteMetadata map[string]string `json:"remote_metadata,omitempty"`
	Title          string            `json:"title,omitempty"`
}

// StreamEvent 是流式输出中的一个事件。
type StreamEvent struct {
	Type    string // "text" | "done" | "error"
	Content string
	Err     error
	// ResponseMessageID 是本轮回复 ID，用于下一轮追问。
	ResponseMessageID int
	// ResponseMessageIDStr 是字符串形式的回复 ID（ChatGPT/Gemini 等平台使用）。
	ResponseMessageIDStr string
	// RemoteConversationID 是平台侧会话 ID（ChatGPT conversation_id / Gemini cid）。
	RemoteConversationID string
	// RemoteMetadata 是本轮响应的平台特定元数据，供下一轮续聊透传。
	RemoteMetadata map[string]string
	// TokenUsage 是本轮累计 token 用量。
	TokenUsage int
	// Title 是平台自动生成的会话标题。
	Title string
}

// RemoteSessionProvider 由需要在平台侧建会话的 provider 实现。
//
// DeepSeek 这类平台的会话 ID 由服务端生成，本地会话必须先换取
// 平台侧 ID 才能发消息。调用方通过类型断言按需使用，
// 不影响只实现 Provider 的平台。
type RemoteSessionProvider interface {
	// CreateRemoteSession 在平台侧创建会话，返回平台会话 ID。
	CreateRemoteSession(ctx context.Context) (string, error)
	// SessionURL 返回平台侧会话的网页地址。
	SessionURL(sessionID string) string
}

// Provider 定义了所有 AI 平台必须实现的接口。
type Provider interface {
	// Name 返回 provider 标识符。
	Name() string

	// Send 发送消息并返回回复。
	Send(ctx context.Context, req *SendRequest) (*SendResponse, error)

	// SendStream 流式发送消息。
	SendStream(ctx context.Context, req *SendRequest) (<-chan StreamEvent, error)

	// Login 执行登录/认证（浏览器模式下由 Driver 完成）。
	Login(ctx context.Context) error

	// ValidateSession 检查当前 cookie/session 是否有效。
	ValidateSession(ctx context.Context) (bool, error)

	// GetSessionURL 返回该 provider 的 Web 会话页面 URL。
	GetSessionURL(sessionID string) string
}

// OutputType 定义输出格式。
type OutputType string

const (
	OutputText     OutputType = "text"
	OutputMarkdown OutputType = "markdown"
	OutputRaw      OutputType = "raw"
)
