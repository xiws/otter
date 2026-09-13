package provider

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/xiws/otter/pkg/api"
)

// DeepSeekProvider 实现 DeepSeek 平台的 Provider 接口。
type DeepSeekProvider struct {
	api       *api.DeepSeekAPI
	authToken string
	account   string
	password  string
}

// NewDeepSeekProvider 创建一个新的 DeepSeek Provider。
func NewDeepSeekProvider() *DeepSeekProvider {
	return &DeepSeekProvider{
		api: api.NewDeepSeekAPI(),
	}
}

// SetAuthToken 设置认证 token。
func (d *DeepSeekProvider) SetAuthToken(token string) {
	d.authToken = token
	d.api.SetAuthToken(token)
}

// SetAccount 设置登录账号。
func (d *DeepSeekProvider) SetAccount(account string) {
	d.account = account
}

// SetPassword 设置登录密码。
func (d *DeepSeekProvider) SetPassword(password string) {
	d.password = password
}

// Token 返回当前的 auth token（登录后获取）。
func (d *DeepSeekProvider) Token() string {
	return d.authToken
}

// Name 返回 provider 标识符。
func (d *DeepSeekProvider) Name() string {
	return "deepseek"
}

// Login 使用账号密码登录 DeepSeek。
// 需要先通过 SetAccount/SetPassword 设置凭据。
func (d *DeepSeekProvider) Login(ctx context.Context) error {
	if d.account == "" {
		return fmt.Errorf("DeepSeek 账号未设置，请先配置 deepseek.account")
	}
	if d.password == "" {
		return fmt.Errorf("DeepSeek 密码未设置，请先配置 deepseek.password 或设置环境变量 OTTER_DEEPSEEK_PASSWORD")
	}

	token, err := d.api.Login(ctx, d.account, d.password)
	if err != nil {
		return fmt.Errorf("DeepSeek 登录失败: %w", err)
	}
	d.authToken = token
	return nil
}

// CreateRemoteSession 在 DeepSeek 平台侧创建会话，返回会话 UUID。
// DeepSeek 的会话 ID 由服务端生成，本地会话必须先换取该 ID 才能发消息。
func (d *DeepSeekProvider) CreateRemoteSession(ctx context.Context) (string, error) {
	if d.authToken == "" {
		return "", fmt.Errorf("DeepSeek 未认证，请先执行 otter auth login deepseek")
	}
	return d.api.CreateSession(ctx)
}

// SessionURL 返回会话在 DeepSeek 网页端的地址。
func (d *DeepSeekProvider) SessionURL(sessionID string) string {
	return d.api.GetSessionURL(sessionID)
}

// Send 发送消息并返回完整回复。
// DeepSeek 只提供 SSE 流式接口，这里收集完整流后返回。
func (d *DeepSeekProvider) Send(ctx context.Context, req *SendRequest) (*SendResponse, error) {
	start := time.Now()

	eventCh, err := d.SendStream(ctx, req)
	if err != nil {
		return nil, err
	}

	var content strings.Builder
	resp := &SendResponse{}
	for ev := range eventCh {
		switch ev.Type {
		case "text":
			content.WriteString(ev.Content)
		case "meta":
			if ev.ResponseMessageID != 0 {
				resp.ResponseMessageID = ev.ResponseMessageID
			}
			if ev.TokenUsage != 0 {
				resp.TokenUsed = ev.TokenUsage
			}
			if ev.Title != "" {
				resp.Title = ev.Title
			}
		case "error":
			if ev.Err != nil {
				return nil, fmt.Errorf("发送消息失败: %w", ev.Err)
			}
			return nil, fmt.Errorf("发送消息失败: %s", ev.Content)
		}
	}

	resp.Content = content.String()
	resp.Duration = time.Since(start)
	return resp, nil
}

// SendStream 流式发送消息。
func (d *DeepSeekProvider) SendStream(ctx context.Context, req *SendRequest) (<-chan StreamEvent, error) {
	if d.authToken == "" {
		return nil, fmt.Errorf("DeepSeek 未认证，请先执行 otter auth login deepseek")
	}
	if req.ChatSessionID == "" {
		return nil, fmt.Errorf("缺少 DeepSeek 会话 ID，请先创建远端会话")
	}

	// 注意：cancel 不能在 SendStream 返回时调用——流由后台 goroutine 消费，
	// 函数返回即 cancel 会立刻中断尚未读取完的响应。改为在流消费结束时释放。
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
	}
	abort := func() {
		if cancel != nil {
			cancel()
		}
	}

	// 1. 上传附件（DeepSeek 要求先上传，再在消息里引用 file id）
	var fileIDs []string
	for _, f := range req.Files {
		if _, err := os.Stat(f.Path); err != nil {
			abort()
			return nil, fmt.Errorf("文件 %s 不存在: %w", f.Path, err)
		}
		resp, err := d.api.UploadFile(ctx, f.Path)
		if err != nil {
			abort()
			return nil, fmt.Errorf("上传文件 %s 失败: %w", f.Path, err)
		}
		fileIDs = append(fileIDs, resp.ID)
	}

	// 2. 发送消息
	chunkCh, err := d.api.Completion(ctx, &api.CompletionRequest{
		ChatSessionID:   req.ChatSessionID,
		ParentMessageID: req.ParentMessageID,
		Prompt:          req.Prompt,
		FileIDs:         fileIDs,
	})
	if err != nil {
		abort()
		return nil, err
	}

	outCh := make(chan StreamEvent, 64)
	go func() {
		defer close(outCh)
		if cancel != nil {
			defer cancel()
		}
		for chunk := range chunkCh {
			switch chunk.Type {
			case "text":
				outCh <- StreamEvent{Type: "text", Content: chunk.Content}
			case "meta":
				outCh <- StreamEvent{
					Type:              "meta",
					ResponseMessageID: chunk.ResponseMessageID,
					TokenUsage:        chunk.TokenUsage,
					Title:             chunk.Title,
				}
			case "done":
				outCh <- StreamEvent{Type: "done"}
			case "error":
				outCh <- StreamEvent{Type: "error", Content: chunk.Content, Err: fmt.Errorf("%s", chunk.Content)}
			}
		}
	}()

	return outCh, nil
}

// ValidateSession 检查当前 token 是否有效。
func (d *DeepSeekProvider) ValidateSession(ctx context.Context) (bool, error) {
	return d.authToken != "", nil
}

// GetSessionURL 返回 DeepSeek 会话页面 URL。
func (d *DeepSeekProvider) GetSessionURL(sessionID string) string {
	return d.api.GetSessionURL(sessionID)
}

// EnsureAuth 确保认证信息已设置。
func (d *DeepSeekProvider) EnsureAuth() error {
	if d.authToken == "" {
		return fmt.Errorf("DeepSeek 未认证，请先执行 otter auth login deepseek")
	}
	return nil
}

// 确保接口一致性。
var (
	_ Provider              = (*DeepSeekProvider)(nil)
	_ RemoteSessionProvider = (*DeepSeekProvider)(nil)
)
