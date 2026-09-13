package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/xiws/otter/pkg/api"
	"github.com/xiws/otter/pkg/chrome"
)

// ChatGPTProvider 实现 ChatGPT 平台的 Provider 接口。
//
// 凭据来自本机 Chrome 的登录 cookie（自动反解），通过 Direct API 直连：
//  1. 登录：从 Chrome 提取 cookie 并保存，换取 access_token 校验
//  2. 发送：POST /backend-api/conversation，流式解析增量文本
//  3. 多轮：持久化 conversation_id 与上一条 message id
type ChatGPTProvider struct {
	api          *api.ChatGPTAPI
	sessionToken string
	accessToken  string
	account      string
	password     string
	cookiesPath  string
}

// NewChatGPTProvider 创建一个新的 ChatGPT Provider。
func NewChatGPTProvider() *ChatGPTProvider {
	return &ChatGPTProvider{
		api: api.NewChatGPTAPI(),
	}
}

// Name 返回 provider 标识符。
func (c *ChatGPTProvider) Name() string {
	return "chatgpt"
}

// SetAccount 设置登录账号（Direct API 模式不再使用，保留兼容）。
func (c *ChatGPTProvider) SetAccount(account string) {
	c.account = account
}

// SetPassword 设置登录密码（Direct API 模式不再使用，保留兼容）。
func (c *ChatGPTProvider) SetPassword(password string) {
	c.password = password
}

// SetCookiesPath 设置 cookie 持久化路径。
func (c *ChatGPTProvider) SetCookiesPath(path string) {
	c.cookiesPath = path
}

// SetModel 设置模型标识（覆盖默认的 auto）。
func (c *ChatGPTProvider) SetModel(model string) {
	c.api.SetModel(model)
}

// SetSessionToken 直接设置 session token（从配置文件恢复的兼容通路）。
func (c *ChatGPTProvider) SetSessionToken(token string) {
	c.sessionToken = token
	c.api.SetSessionToken(token)
}

// SetAccessToken 直接设置 access token。
func (c *ChatGPTProvider) SetAccessToken(token string) {
	c.accessToken = token
	c.api.SetAccessToken(token)
}

// Token 返回当前 access token（如果有）。
func (c *ChatGPTProvider) Token() string {
	if c.accessToken != "" {
		return c.accessToken
	}
	return c.sessionToken
}

// Account 返回当前登录账号（邮箱），未登录时为空。
func (c *ChatGPTProvider) Account() string {
	return c.account
}

// Login 从本机 Chrome 提取 ChatGPT cookie 并校验登录态。
func (c *ChatGPTProvider) Login(ctx context.Context) error {
	cookies, err := chrome.ExtractCookies("chatgpt.com", "openai.com")
	if err != nil {
		return fmt.Errorf("从 Chrome 提取 ChatGPT cookies 失败（请确认 Chrome 中已登录 chatgpt.com）: %w", err)
	}

	c.api.SetCookies(cookies)
	session, err := c.api.Session(ctx)
	if err != nil {
		return fmt.Errorf("ChatGPT 登录态校验失败: %w", err)
	}

	if c.cookiesPath != "" {
		if err := chrome.SaveCookies(c.cookiesPath, cookies); err != nil {
			return fmt.Errorf("保存 cookies 失败: %w", err)
		}
	}

	c.accessToken = session.AccessToken
	if session.User != nil && session.User.Email != "" {
		c.account = session.User.Email
	}
	return nil
}

// Send 发送消息并返回完整回复。
func (c *ChatGPTProvider) Send(ctx context.Context, req *SendRequest) (*SendResponse, error) {
	start := time.Now()

	ch, err := c.startStream(ctx, req)
	if err != nil {
		return nil, err
	}

	res := &chatGPTTurnResult{}
	for chunk := range ch {
		res.apply(chunk)
		if chunk.Type == "error" {
			if chunk.Err != nil {
				return nil, fmt.Errorf("接收回复失败: %w", chunk.Err)
			}
			return nil, fmt.Errorf("接收回复失败: %s", chunk.Content)
		}
	}

	return &SendResponse{
		Content:              res.content,
		Duration:             time.Since(start),
		RemoteConversationID: res.conversationID,
		ResponseMessageIDStr: res.messageID,
	}, nil
}

// SendStream 流式发送消息。
func (c *ChatGPTProvider) SendStream(ctx context.Context, req *SendRequest) (<-chan StreamEvent, error) {
	ch, err := c.startStream(ctx, req)
	if err != nil {
		return nil, err
	}

	outCh := make(chan StreamEvent, 64)
	go func() {
		defer close(outCh)

		emit := func(ev StreamEvent) bool {
			select {
			case outCh <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		res := &chatGPTTurnResult{}
		for chunk := range ch {
			switch chunk.Type {
			case "text":
				res.content += chunk.Content
				if !emit(StreamEvent{Type: "text", Content: chunk.Content}) {
					return
				}
			case "meta", "done":
				res.apply(chunk)
			case "error":
				emit(StreamEvent{Type: "error", Content: chunk.Err.Error(), Err: chunk.Err})
				return
			}
		}
		emit(StreamEvent{
			Type:                 "done",
			RemoteConversationID: res.conversationID,
			ResponseMessageIDStr: res.messageID,
		})
	}()

	return outCh, nil
}

// ValidateSession 检查当前登录态是否有效（真实网络校验）。
func (c *ChatGPTProvider) ValidateSession(ctx context.Context) (bool, error) {
	if err := c.ensureAuthenticated(ctx); err != nil {
		return false, err
	}
	session, err := c.api.Session(ctx)
	if err != nil {
		return false, err
	}
	if session.User != nil && session.User.Email != "" {
		c.account = session.User.Email
	}
	return true, nil
}

// GetSessionURL 返回 ChatGPT 会话页面 URL。
func (c *ChatGPTProvider) GetSessionURL(sessionID string) string {
	return c.api.GetSessionURL(sessionID)
}

// --- 内部实现 ---

// chatGPTTurnResult 汇总一轮对话的流式结果。
type chatGPTTurnResult struct {
	content        string
	conversationID string
	messageID      string
}

// apply 记录 meta/done 事件中的会话标识。
func (r *chatGPTTurnResult) apply(chunk api.ChatGPTStreamChunk) {
	switch chunk.Type {
	case "text":
		r.content += chunk.Content
	case "meta", "done":
		if chunk.ConversationID != "" {
			r.conversationID = chunk.ConversationID
		}
		if chunk.MessageID != "" {
			r.messageID = chunk.MessageID
		}
	}
}

// ensureAuthenticated 准备可用的登录凭据。
//
// 凭据优先级：完整 cookie（持久化文件 / 本机 Chrome 提取）
// > 手动 session token > 直接 access token（均兼容旧配置）。
func (c *ChatGPTProvider) ensureAuthenticated(ctx context.Context) error {
	c.api.SetCookieRefresher(func() ([]chrome.Cookie, error) {
		return chrome.ExtractCookies("chatgpt.com", "openai.com")
	})

	cookies, extractErr := c.resolveCookies()
	switch {
	case extractErr == nil && len(cookies) > 0:
		c.api.SetCookies(cookies)
	case c.sessionToken != "":
		c.api.SetSessionToken(c.sessionToken)
	case c.accessToken != "":
		// 直接 access token 通路无需预校验
		c.api.SetAccessToken(c.accessToken)
		return nil
	default:
		return fmt.Errorf("ChatGPT 未认证：无法获取登录 cookie（%v）。请确认 Chrome 已登录 chatgpt.com 后执行 otter auth login gpt", extractErr)
	}

	// 预取 access_token，尽早发现过期登录态
	if _, err := c.api.GetAccessToken(ctx); err != nil {
		return fmt.Errorf("ChatGPT 登录校验失败: %w。请确认 Chrome 已登录 chatgpt.com，或执行 otter auth login gpt", err)
	}
	return nil
}

// resolveCookies 优先读取持久化 cookie，缺失时从 Chrome 提取并缓存。
func (c *ChatGPTProvider) resolveCookies() ([]chrome.Cookie, error) {
	if c.cookiesPath != "" {
		if cookies, err := chrome.LoadCookies(c.cookiesPath); err == nil && len(cookies) > 0 {
			return cookies, nil
		}
	}

	cookies, err := chrome.ExtractCookies("chatgpt.com", "openai.com")
	if err != nil {
		return nil, err
	}
	if c.cookiesPath != "" {
		// 缓存写入失败不影响本次使用
		_ = chrome.SaveCookies(c.cookiesPath, cookies)
	}
	return cookies, nil
}

// startStream 完成认证并提交一轮对话。
func (c *ChatGPTProvider) startStream(ctx context.Context, req *SendRequest) (<-chan api.ChatGPTStreamChunk, error) {
	if len(req.Files) > 0 {
		return nil, fmt.Errorf("ChatGPT 附件上传尚未支持，请先移除文件参数")
	}

	if err := c.ensureAuthenticated(ctx); err != nil {
		return nil, err
	}

	return c.api.Send(ctx, req.Prompt, req.ChatSessionID, req.ParentMessageIDStr)
}

// 确保接口一致性。
var _ Provider = (*ChatGPTProvider)(nil)
