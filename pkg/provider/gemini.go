package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"otter/pkg/api"
	"otter/pkg/chrome"
)

// GeminiProvider 实现 Gemini 平台的 Provider 接口。
//
// 通过 Direct API 直连 Gemini Web 的 StreamGenerate 协议：
//  1. 从本机 Chrome 反解 google.com 登录 cookie
//  2. 打开 /app 提取 SNlM0e 等令牌
//  3. 以长度前缀帧协议发送消息并解析流式回复
type GeminiProvider struct {
	account     string
	password    string
	cookiesPath string
	language    string
	api         *api.GeminiAPI
}

// NewGeminiProvider 创建一个新的 Gemini Provider。
func NewGeminiProvider() *GeminiProvider {
	return &GeminiProvider{}
}

// Name 返回 provider 标识符。
func (g *GeminiProvider) Name() string {
	return "gemini"
}

// SetAccount 设置登录账号。
func (g *GeminiProvider) SetAccount(account string) {
	g.account = account
}

// SetPassword 设置登录密码（Direct API 模式不再使用，保留兼容）。
func (g *GeminiProvider) SetPassword(password string) {
	g.password = password
}

// SetCookiesPath 设置 cookie 持久化路径。
func (g *GeminiProvider) SetCookiesPath(path string) {
	g.cookiesPath = path
}

// SetLanguage 设置请求语言（如 en / zh-CN），空串表示使用默认值。
func (g *GeminiProvider) SetLanguage(lang string) {
	g.language = lang
}

// Login 从本机 Chrome 提取 Google cookie 并校验 Gemini 登录态。
func (g *GeminiProvider) Login(ctx context.Context) error {
	cookies, err := chrome.ExtractCookies("google.com")
	if err != nil {
		return fmt.Errorf("从 Chrome 提取 Google cookies 失败（请确认 Chrome 中已登录 gemini.google.com）: %w", err)
	}

	client := api.NewGeminiAPI()
	if g.language != "" {
		client.SetLanguage(g.language)
	}
	client.SetCookies(cookies)
	if err := client.Init(ctx); err != nil {
		return fmt.Errorf("Gemini 登录态校验失败: %w", err)
	}

	if g.cookiesPath != "" {
		if err := chrome.SaveCookies(g.cookiesPath, cookies); err != nil {
			return fmt.Errorf("保存 cookies 失败: %w", err)
		}
	}
	g.api = client
	return nil
}

// geminiEmptyResponseRetries 表示空响应（无文本无错误）时的重试上限。
const geminiEmptyResponseRetries = 1

// Send 发送消息并等待完整回复。
func (g *GeminiProvider) Send(ctx context.Context, req *SendRequest) (*SendResponse, error) {
	start := time.Now()

	ch, err := g.SendStream(ctx, req)
	if err != nil {
		return nil, err
	}

	res := &SendResponse{}
	for ev := range ch {
		switch ev.Type {
		case "text":
			res.Content += ev.Content
		case "done":
			res.RemoteConversationID = ev.RemoteConversationID
			res.ResponseMessageIDStr = ev.ResponseMessageIDStr
			res.RemoteMetadata = ev.RemoteMetadata
		case "error":
			if ev.Err != nil {
				return nil, ev.Err
			}
			return nil, fmt.Errorf("%s", ev.Content)
		}
	}
	res.Duration = time.Since(start)
	return res, nil
}

// SendStream 流式发送消息。
//
// 若服务端返回空响应（既无文本也无错误），自动刷新客户端令牌并重试，
// 与参考实现的重试机制对齐，避免偶发空回复直接暴露给用户。
func (g *GeminiProvider) SendStream(ctx context.Context, req *SendRequest) (<-chan StreamEvent, error) {
	if len(req.Files) > 0 {
		return nil, fmt.Errorf("Gemini 附件上传尚未支持，请先移除文件参数")
	}

	// 预检客户端可用性，尽早暴露凭据问题
	if _, err := g.ensureClient(ctx); err != nil {
		return nil, err
	}

	var metadata []any
	if raw := req.RemoteMetadata["gemini_metadata"]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			metadata = nil // 元数据损坏时按新会话处理
		}
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

		res := &geminiTurnResult{}
		for attempt := 0; ; attempt++ {
			client, err := g.ensureClient(ctx)
			if err != nil {
				emit(StreamEvent{Type: "error", Content: err.Error(), Err: err})
				return
			}

			ch, err := client.StreamGenerate(ctx, req.Prompt, metadata)
			if err != nil {
				emit(StreamEvent{Type: "error", Content: err.Error(), Err: err})
				return
			}

			for chunk := range ch {
				switch chunk.Type {
				case "text":
					res.content += chunk.Content
					if !emit(StreamEvent{Type: "text", Content: chunk.Content}) {
						return
					}
				case "meta":
					res.applyMeta(chunk)
				case "error":
					emit(StreamEvent{Type: "error", Content: chunk.Err.Error(), Err: chunk.Err})
					return
				}
			}

			if res.content != "" {
				emit(StreamEvent{
					Type:                 "done",
					RemoteConversationID: res.conversationID,
					ResponseMessageIDStr: res.responseID,
					RemoteMetadata:       res.metadataMap(),
				})
				return
			}

			// 空响应：刷新客户端（重新打开页面提取令牌）后重试
			if attempt >= geminiEmptyResponseRetries {
				emit(StreamEvent{Type: "error", Content: "Gemini 未返回文本内容，请稍后重试", Err: fmt.Errorf("Gemini 未返回文本内容")})
				return
			}
			g.resetClient()
		}
	}()

	return outCh, nil
}

// ValidateSession 检查当前 cookie/session 是否有效（真实网络校验）。
func (g *GeminiProvider) ValidateSession(ctx context.Context) (bool, error) {
	if _, err := g.ensureClient(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// GetSessionURL 返回 Gemini 会话页面 URL。
func (g *GeminiProvider) GetSessionURL(sessionID string) string {
	if sessionID != "" {
		return fmt.Sprintf("https://gemini.google.com/app/%s", sessionID)
	}
	return "https://gemini.google.com/app"
}

// --- 内部实现 ---

// geminiTurnResult 汇总一轮对话的流式结果。
type geminiTurnResult struct {
	content        string
	conversationID string
	responseID     string
	metadata       []any
}

// applyMeta 记录 meta 事件中的会话标识与续聊元数据。
func (r *geminiTurnResult) applyMeta(chunk api.GeminiChunk) {
	if chunk.ConversationID != "" {
		r.conversationID = chunk.ConversationID
	}
	if chunk.ResponseID != "" {
		r.responseID = chunk.ResponseID
	}
	if chunk.Metadata != nil {
		r.metadata = chunk.Metadata
	}
}

// metadataMap 将续聊元数据序列化为待持久化的键值对。
func (r *geminiTurnResult) metadataMap() map[string]string {
	if r.metadata == nil {
		return nil
	}
	raw, err := json.Marshal(r.metadata)
	if err != nil {
		return nil
	}
	return map[string]string{"gemini_metadata": string(raw)}
}

// ensureClient 返回可用的 API 客户端，必要时初始化令牌。
func (g *GeminiProvider) ensureClient(ctx context.Context) (*api.GeminiAPI, error) {
	if g.api != nil && g.api.AccessToken() != "" {
		return g.api, nil
	}

	cookies, err := g.resolveCookies()
	if err != nil {
		return nil, err
	}

	client := api.NewGeminiAPI()
	if g.language != "" {
		client.SetLanguage(g.language)
	}
	client.SetCookies(cookies)
	if err := client.Init(ctx); err != nil {
		// 持久化 cookie 可能已过期：重新从 Chrome 提取一次再试
		fresh, exErr := chrome.ExtractCookies("google.com")
		if exErr != nil {
			return nil, err
		}
		client.SetCookies(fresh)
		if retryErr := client.Init(ctx); retryErr != nil {
			return nil, err
		}
		if g.cookiesPath != "" {
			_ = chrome.SaveCookies(g.cookiesPath, fresh)
		}
	}

	g.api = client
	return client, nil
}

// resetClient 丢弃当前客户端，使下次 ensureClient 重新初始化令牌。
func (g *GeminiProvider) resetClient() {
	g.api = nil
}

// resolveCookies 优先读取持久化 cookie，缺失时从 Chrome 提取并保存。
func (g *GeminiProvider) resolveCookies() ([]chrome.Cookie, error) {
	if g.cookiesPath != "" {
		if cookies, err := chrome.LoadCookies(g.cookiesPath); err == nil && len(cookies) > 0 {
			return cookies, nil
		}
	}

	cookies, err := chrome.ExtractCookies("google.com")
	if err != nil {
		return nil, fmt.Errorf("从 Chrome 提取 Google cookies 失败（请确认 Chrome 中已登录 gemini.google.com）: %w", err)
	}
	if g.cookiesPath != "" {
		if err := chrome.SaveCookies(g.cookiesPath, cookies); err != nil {
			return nil, fmt.Errorf("保存 cookies 失败: %w", err)
		}
	}
	return cookies, nil
}

// 确保接口一致性。
var _ Provider = (*GeminiProvider)(nil)
