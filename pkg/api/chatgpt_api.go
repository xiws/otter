// Package api 提供与各 AI 平台的 HTTP API 交互实现。
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"otter/pkg/chrome"
)

// ChatGPT Web 接口路径。
const (
	baseURLChatGPT            = "https://chatgpt.com"
	chatGPTSessionPath        = "/api/auth/session"
	chatGPTModelsPath         = "/backend-api/models"
	chatGPTConversationPath   = "/backend-api/f/conversation"
	chatGPTConduitPreparePath = "/backend-api/f/conversation/prepare"

	// ChatGPT 客户端版本标识（用于 OAI-Client-Version 等身份头）。
	chatGPTClientVersion     = "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"
	chatGPTClientBuildNumber = "6708908"
)

// chatGPTRootMessageID 是新会话首轮的父消息哨兵值。
const chatGPTRootMessageID = "client-created-root"

// chatGPTDefaultModel 是默认模型标识（由服务端自动路由）。
const chatGPTDefaultModel = "auto"

// chatGPTStreamTimeout 是流式请求的整体超时上限。
const chatGPTStreamTimeout = 10 * time.Minute

// errChatGPTUnauthorized 表示登录态缺失或已过期。
var errChatGPTUnauthorized = errors.New("ChatGPT 未登录或登录态已过期")

// ChatGPTAPI 封装 ChatGPT Web 的 Direct API 调用。
//
// Cloudflare 会拦截标准 net/http 客户端，因此统一使用 tls-client 的
// Chrome 指纹传输；认证依赖从浏览器提取的完整 cookie 集：
//  1. SetCookies / SetSessionToken 提供凭据
//  2. GetAccessToken 通过 /api/auth/session 换取 access_token（JWT）
//  3. Send 依次完成 sentinel 反爬握手（含 PoW）、conduit 预热，
//     最后以 Bearer + cookie + OpenAI-Sentinel-* 头请求
//     /backend-api/f/conversation 获取流式回复
type ChatGPTAPI struct {
	client        tls_client.HttpClient
	cookieHdr     string
	sessionToken  string
	accessToken   string
	model         string
	modelExplicit bool
	refresher     func() ([]chrome.Cookie, error)
	deviceID      string
	oaiSessionID  string
	mu            sync.Mutex

	// bootstrap 资源缓存（每进程仅拉取一次首页）
	sentinelOnce    sync.Once
	sentinelScripts []string
	sentinelBuild   string
}

// NewChatGPTAPI 创建一个新的 ChatGPT API 客户端。
func NewChatGPTAPI() *ChatGPTAPI {
	client, err := newBrowserClient(chatGPTStreamTimeout)
	if err != nil {
		// newBrowserClient 仅在配置非法时失败，保留降级空间
		client = nil
	}
	return &ChatGPTAPI{
		client:       client,
		model:        chatGPTDefaultModel,
		deviceID:     newUUIDv4(),
		oaiSessionID: newUUIDv4(),
	}
}

// SetCookies 设置从 Chrome 提取的完整 cookie 集。
func (c *ChatGPTAPI) SetCookies(cookies []chrome.Cookie) {
	c.cookieHdr = chrome.CookieHeader(cookies)
}

// SetSessionToken 设置 ChatGPT 的 session token。
//
// 兼容旧配置的手动通路；新流程优先使用 SetCookies 提供的完整 cookie 集。
func (c *ChatGPTAPI) SetSessionToken(token string) {
	c.sessionToken = token
	c.cookieHdr = "__Secure-next-auth.session-token=" + token
}

// GetSessionToken 返回当前 session token。
func (c *ChatGPTAPI) GetSessionToken() string {
	return c.sessionToken
}

// SetAccessToken 直接设置 access_token（跳过 session 换取）。
func (c *ChatGPTAPI) SetAccessToken(token string) {
	c.mu.Lock()
	c.accessToken = token
	c.mu.Unlock()
}

// SetModel 设置模型标识；空串表示保持默认（auto）。
func (c *ChatGPTAPI) SetModel(model string) {
	if model != "" {
		c.model = model
		c.modelExplicit = true
	}
}

// SetCookieRefresher 设置 cookie 重新提取回调。
//
// 当 access_token 换取返回 401 时会调用该回调刷新 cookie 并重试一次。
func (c *ChatGPTAPI) SetCookieRefresher(fn func() ([]chrome.Cookie, error)) {
	c.refresher = fn
}

// ChatGPTSessionResponse 是 /api/auth/session 的返回结构。
type ChatGPTSessionResponse struct {
	User        *ChatGPTUser `json:"user,omitempty"`
	Expires     string       `json:"expires,omitempty"`
	AccessToken string       `json:"accessToken,omitempty"`
	Error       string       `json:"error,omitempty"`
}

// ChatGPTUser 是 ChatGPT 用户信息。
type ChatGPTUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Image string `json:"image"`
	Plan  string `json:"plan,omitempty"`
}

// Session 获取当前登录会话信息（含 access_token）。
//
// 登录态失效且配置了 refresher 时，会重新提取 cookie 再试一次。
func (c *ChatGPTAPI) Session(ctx context.Context) (*ChatGPTSessionResponse, error) {
	session, err := c.fetchSession(ctx)
	if err == nil {
		return session, nil
	}
	if !errors.Is(err, errChatGPTUnauthorized) || c.refresher == nil {
		return nil, err
	}
	cookies, refreshErr := c.refresher()
	if refreshErr != nil || len(cookies) == 0 {
		return nil, err
	}
	c.SetCookies(cookies)
	return c.fetchSession(ctx)
}

// fetchSession 执行一次 /api/auth/session 请求。
func (c *ChatGPTAPI) fetchSession(ctx context.Context) (*ChatGPTSessionResponse, error) {
	if c.client == nil {
		return nil, fmt.Errorf("ChatGPT HTTP 客户端初始化失败")
	}
	if c.cookieHdr == "" {
		return nil, fmt.Errorf("ChatGPT cookie 未设置，请先执行 otter auth login gpt")
	}

	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, baseURLChatGPT+chatGPTSessionPath, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	c.applyIdentityHeaders(req)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 session 失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 session 响应失败: %w", err)
	}
	if resp.StatusCode == fhttp.StatusUnauthorized || resp.StatusCode == fhttp.StatusForbidden {
		return nil, fmt.Errorf("%w (HTTP %d)", errChatGPTUnauthorized, resp.StatusCode)
	}
	if resp.StatusCode != fhttp.StatusOK {
		return nil, fmt.Errorf("获取 session 失败: HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}

	var session ChatGPTSessionResponse
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, fmt.Errorf("解析 session 响应失败: %w", err)
	}
	if session.Error != "" {
		return nil, fmt.Errorf("%w: %s", errChatGPTUnauthorized, session.Error)
	}
	if session.AccessToken == "" {
		return nil, fmt.Errorf("%w: session 响应缺少 accessToken", errChatGPTUnauthorized)
	}

	c.mu.Lock()
	c.accessToken = session.AccessToken
	c.mu.Unlock()
	return &session, nil
}

// GetAccessToken 换取 access_token（兼容旧调用方）。
func (c *ChatGPTAPI) GetAccessToken(ctx context.Context) (string, error) {
	session, err := c.Session(ctx)
	if err != nil {
		return "", fmt.Errorf("获取 access token 失败: %w", err)
	}
	return session.AccessToken, nil
}

// bearerToken 返回可用的 access_token（优先缓存，缺失时自动换取）。
func (c *ChatGPTAPI) bearerToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	token := c.accessToken
	c.mu.Unlock()
	if token != "" {
		return token, nil
	}
	return c.GetAccessToken(ctx)
}

// refreshAccessToken 清除缓存并重新换取 access_token。
func (c *ChatGPTAPI) refreshAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	c.accessToken = ""
	c.mu.Unlock()
	return c.GetAccessToken(ctx)
}

// chatGPTModelsResponse 是 /backend-api/models 的返回结构。
type chatGPTModelsResponse struct {
	Models []struct {
		Slug  string `json:"slug"`
		Title string `json:"title"`
	} `json:"models"`
}

// GetModels 返回账号可用模型的 slug 列表。
func (c *ChatGPTAPI) GetModels(ctx context.Context) ([]string, error) {
	token, err := c.bearerToken(ctx)
	if err != nil {
		return nil, err
	}

	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, baseURLChatGPT+chatGPTModelsPath, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	c.applyIdentityHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取模型列表失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取模型列表失败: %w", err)
	}
	if resp.StatusCode != fhttp.StatusOK {
		return nil, fmt.Errorf("获取模型列表失败: HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}

	var parsed chatGPTModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %w", err)
	}
	slugs := make([]string, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		if m.Slug != "" {
			slugs = append(slugs, m.Slug)
		}
	}
	return slugs, nil
}

// chatGPTConversationRequest 是 /backend-api/f/conversation 的请求体。
type chatGPTConversationRequest struct {
	Action                   string                `json:"action"`
	Messages                 []chatGPTMessage      `json:"messages"`
	ParentMessageID          string                `json:"parent_message_id"`
	ConversationID           string                `json:"conversation_id,omitempty"`
	Model                    string                `json:"model"`
	ClientPrepareState       string                `json:"client_prepare_state"`
	TimezoneOffsetMin        int                   `json:"timezone_offset_min"`
	Timezone                 string                `json:"timezone"`
	ConversationMode         map[string]string     `json:"conversation_mode"`
	EnableMessageFollowups   bool                  `json:"enable_message_followups"`
	SystemHints              []string              `json:"system_hints"`
	SupportsBuffering        bool                  `json:"supports_buffering"`
	SupportedEncodings       []string              `json:"supported_encodings"`
	ClientContextualInfo     chatGPTContextualInfo `json:"client_contextual_info"`
	ParagenCotSummaryDisplay string                `json:"paragen_cot_summary_display_override"`
	ForceParallelSwitch      string                `json:"force_parallel_switch"`
}

// chatGPTContextualInfo 模拟浏览器页面上下文信息。
type chatGPTContextualInfo struct {
	IsDarkMode      bool   `json:"is_dark_mode"`
	TimeSinceLoaded int    `json:"time_since_loaded"`
	PageHeight      int    `json:"page_height"`
	PageWidth       int    `json:"page_width"`
	PixelRatio      int    `json:"pixel_ratio"`
	ScreenHeight    int    `json:"screen_height"`
	ScreenWidth     int    `json:"screen_width"`
	AppName         string `json:"app_name"`
}

// chatGPTPrepareRequest 是 /backend-api/f/conversation/prepare 的请求体。
//
// 用于换取 x-conduit-token；除用 partial_query 代替 messages 外，
// 字段与 conversation 请求基本一致。
type chatGPTPrepareRequest struct {
	Action               string            `json:"action"`
	ForkFromSharedPost   bool              `json:"fork_from_shared_post"`
	ParentMessageID      string            `json:"parent_message_id"`
	ConversationID       string            `json:"conversation_id,omitempty"`
	Model                string            `json:"model"`
	ClientPrepareState   string            `json:"client_prepare_state"`
	TimezoneOffsetMin    int               `json:"timezone_offset_min"`
	Timezone             string            `json:"timezone"`
	ConversationMode     map[string]string `json:"conversation_mode"`
	SystemHints          []string          `json:"system_hints"`
	PartialQuery         chatGPTMessage    `json:"partial_query"`
	SupportsBuffering    bool              `json:"supports_buffering"`
	SupportedEncodings   []string          `json:"supported_encodings"`
	ClientContextualInfo map[string]string `json:"client_contextual_info"`
}

// chatGPTMessage 是请求体中的消息结构。
type chatGPTMessage struct {
	ID         string                  `json:"id"`
	Author     chatGPTMessageAuthor    `json:"author"`
	CreateTime float64                 `json:"create_time,omitempty"`
	Content    chatGPTMessageContent   `json:"content"`
	Metadata   *chatGPTMessageMetadata `json:"metadata,omitempty"`
}

// chatGPTMessageMetadata 模拟前端附带的会话元数据。
type chatGPTMessageMetadata struct {
	DeveloperModeConnectorIDs []string `json:"developer_mode_connector_ids"`
	SelectedSources           []string `json:"selected_sources"`
	SelectedGitHubRepos       []string `json:"selected_github_repos"`
	SelectedAllGitHubRepos    bool     `json:"selected_all_github_repos"`
	SerializationMetadata     struct {
		CustomSymbolOffsets []any `json:"custom_symbol_offsets"`
	} `json:"serialization_metadata"`
}

// chatGPTMessageAuthor 是消息作者。
type chatGPTMessageAuthor struct {
	Role string `json:"role"`
}

// chatGPTMessageContent 是消息内容。
type chatGPTMessageContent struct {
	ContentType string   `json:"content_type"`
	Parts       []string `json:"parts"`
}

// ChatGPTStreamChunk 是 ChatGPT SSE 流中的一个事件。
type ChatGPTStreamChunk struct {
	Type           string // "text" | "meta" | "done" | "error"
	Content        string
	Err            error
	MessageID      string
	ConversationID string
}

// Send 发送消息并返回流式事件。
//
// 每轮依次执行 sentinel 反爬握手（含 PoW）、conduit 预热，然后请求
// /backend-api/f/conversation 获取 SSE 流。
// parentMessageID 为空表示新会话（使用哨兵父消息）；
// conversationID 为空表示新会话，追问时需携带上一轮捕获的 conversation_id。
func (c *ChatGPTAPI) Send(ctx context.Context, prompt, conversationID, parentMessageID string) (<-chan ChatGPTStreamChunk, error) {
	if c.client == nil {
		return nil, fmt.Errorf("ChatGPT HTTP 客户端初始化失败")
	}
	if c.cookieHdr == "" {
		return nil, fmt.Errorf("ChatGPT cookie 未设置，请先执行 otter auth login gpt")
	}
	if parentMessageID == "" {
		parentMessageID = chatGPTRootMessageID
	}

	resp, err := c.sendOnce(ctx, prompt, conversationID, parentMessageID, c.model)
	if err != nil {
		return nil, err
	}

	// 模型兜底：默认模型被服务端拒绝时，改用账号模型列表的首个 slug 重试一次
	if resp.StatusCode != fhttp.StatusOK && !c.modelExplicit {
		status := resp.StatusCode
		errBody := drainBody(resp)
		slug := c.firstModelSlug(ctx)
		if slug == "" || slug == c.model {
			return nil, fmt.Errorf("ChatGPT 返回 HTTP %d: %s", status, truncateStr(string(errBody), 500))
		}
		retryResp, retryErr := c.sendOnce(ctx, prompt, conversationID, parentMessageID, slug)
		if retryErr != nil {
			return nil, retryErr
		}
		if retryResp.StatusCode != fhttp.StatusOK {
			retryBody := drainBody(retryResp)
			return nil, fmt.Errorf("ChatGPT 返回 HTTP %d: %s", retryResp.StatusCode, truncateStr(string(retryBody), 500))
		}
		resp = retryResp
	} else if resp.StatusCode != fhttp.StatusOK {
		status := resp.StatusCode
		errBody := drainBody(resp)
		return nil, fmt.Errorf("ChatGPT 返回 HTTP %d: %s", status, truncateStr(string(errBody), 500))
	}

	ch := make(chan ChatGPTStreamChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		c.parseSSE(ctx, resp.Body, ch)
	}()
	return ch, nil
}

// sendOnce 执行一次完整发送：sentinel 握手 → conduit 预热 → conversation 请求。
//
// 401 时自动刷新登录态并重建全部临时令牌后重试一次。
func (c *ChatGPTAPI) sendOnce(ctx context.Context, prompt, conversationID, parentMessageID, model string) (*fhttp.Response, error) {
	sentinel, err := c.fetchSentinel(ctx)
	if err != nil {
		return nil, fmt.Errorf("ChatGPT 反爬校验失败: %w", err)
	}
	payload := c.buildConversationPayload(prompt, conversationID, parentMessageID, model)

	conduitToken, err := c.prepareConduit(ctx, payload)
	if err != nil {
		return nil, err
	}

	// 调试开关：导出 sentinel/conduit 令牌供 A/B 对照实验使用
	if dumpPath := os.Getenv("OTTER_SENTINEL_DUMP"); dumpPath != "" {
		body, _ := json.Marshal(payload)
		dump, _ := json.Marshal(map[string]string{
			"token":           sentinel.RequirementsToken,
			"proof_token":     sentinel.ProofToken,
			"turnstile_token": sentinel.TurnstileToken,
			"so_token":        sentinel.SOToken,
			"conduit_token":   conduitToken,
			"p_token":         sentinel.pToken,
			"pow_seed":        sentinel.powSeed,
			"pow_difficulty":  sentinel.powDifficulty,
			"body":            string(body),
			"device_id":       c.deviceID,
			"session_id":      c.oaiSessionID,
		})
		if err := os.WriteFile(dumpPath, dump, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "[sentinel] dump 失败: %v\n", err)
		}
	}

	resp, err := c.postConversation(ctx, payload, sentinel, conduitToken)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != fhttp.StatusUnauthorized {
		return resp, nil
	}

	// 登录态失效：刷新 access_token 与全部临时令牌后重试一次
	resp.Body.Close()
	if _, err := c.refreshAccessToken(ctx); err != nil {
		return nil, fmt.Errorf("ChatGPT 登录态已失效，请重新执行 otter auth login gpt: %w", err)
	}
	sentinel, err = c.fetchSentinel(ctx)
	if err != nil {
		return nil, fmt.Errorf("ChatGPT 反爬校验失败: %w", err)
	}
	conduitToken, err = c.prepareConduit(ctx, payload)
	if err != nil {
		return nil, err
	}
	return c.postConversation(ctx, payload, sentinel, conduitToken)
}

// buildConversationPayload 构造一轮对话的请求体。
func (c *ChatGPTAPI) buildConversationPayload(prompt, conversationID, parentMessageID, model string) chatGPTConversationRequest {
	msg := chatGPTMessage{
		ID:         newUUIDv4(),
		Author:     chatGPTMessageAuthor{Role: "user"},
		CreateTime: float64(time.Now().UnixNano()) / 1e9,
		Content: chatGPTMessageContent{
			ContentType: "text",
			Parts:       []string{prompt},
		},
	}
	msg.Metadata = &chatGPTMessageMetadata{
		DeveloperModeConnectorIDs: []string{},
		SelectedSources:           []string{},
		SelectedGitHubRepos:       []string{},
	}
	msg.Metadata.SerializationMetadata.CustomSymbolOffsets = []any{}
	return chatGPTConversationRequest{
		Action:                   "next",
		Messages:                 []chatGPTMessage{msg},
		ParentMessageID:          parentMessageID,
		ConversationID:           conversationID,
		Model:                    model,
		ClientPrepareState:       "success",
		TimezoneOffsetMin:        -480,
		Timezone:                 "Asia/Shanghai",
		ConversationMode:         map[string]string{"kind": "primary_assistant"},
		EnableMessageFollowups:   true,
		SystemHints:              []string{},
		SupportsBuffering:        true,
		SupportedEncodings:       []string{"v1"},
		ParagenCotSummaryDisplay: "allow",
		ForceParallelSwitch:      "auto",
		ClientContextualInfo: chatGPTContextualInfo{
			IsDarkMode:      false,
			TimeSinceLoaded: 120,
			PageHeight:      900,
			PageWidth:       1400,
			PixelRatio:      2,
			ScreenHeight:    1440,
			ScreenWidth:     2560,
			AppName:         "chatgpt.com",
		},
	}
}

// prepareConduit 预热 conversation 端点并换取 x-conduit-token。
func (c *ChatGPTAPI) prepareConduit(ctx context.Context, base chatGPTConversationRequest) (string, error) {
	token, err := c.bearerToken(ctx)
	if err != nil {
		return "", err
	}
	// partial_query 只保留 id/author/content（与浏览器首次提交的结构一致，
	// 不带 create_time/metadata 等完整消息字段）。
	partial := base.Messages[0]
	partial.Metadata = nil
	partial.CreateTime = 0
	partial.ID = newUUIDv4()
	prepare := chatGPTPrepareRequest{
		Action:               "next",
		ForkFromSharedPost:   false,
		ParentMessageID:      base.ParentMessageID,
		ConversationID:       base.ConversationID,
		Model:                base.Model,
		ClientPrepareState:   "none",
		TimezoneOffsetMin:    base.TimezoneOffsetMin,
		Timezone:             base.Timezone,
		ConversationMode:     base.ConversationMode,
		SystemHints:          []string{},
		PartialQuery:         partial,
		SupportsBuffering:    true,
		SupportedEncodings:   []string{"v1"},
		ClientContextualInfo: map[string]string{"app_name": "chatgpt.com"},
	}
	body, err := json.Marshal(prepare)
	if err != nil {
		return "", fmt.Errorf("序列化 conduit 请求失败: %w", err)
	}

	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodPost, baseURLChatGPT+chatGPTConduitPreparePath, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("创建请求失败: %w", err)
	}
	c.applyIdentityHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", baseURLChatGPT)
	req.Header.Set("Referer", baseURLChatGPT+"/")
	req.Header.Set("X-Conduit-Token", "no-token")
	req.Header.Set("X-OpenAI-Target-Path", chatGPTConduitPreparePath)
	req.Header.Set("X-OpenAI-Target-Route", chatGPTConduitPreparePath)

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("conduit 预热请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("读取 conduit 响应失败: %w", err)
	}
	if resp.StatusCode != fhttp.StatusOK {
		return "", fmt.Errorf("conduit 预热失败: HTTP %d: %s", resp.StatusCode, truncateStr(string(data), 300))
	}
	var parsed struct {
		ConduitToken string `json:"conduit_token"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("解析 conduit 响应失败: %w", err)
	}
	if parsed.ConduitToken == "" {
		return "", fmt.Errorf("conduit 预热响应缺少 conduit_token")
	}
	return parsed.ConduitToken, nil
}

// postConversation 提交一轮对话请求（含全部 sentinel 与 conduit 请求头）。
func (c *ChatGPTAPI) postConversation(ctx context.Context, payload chatGPTConversationRequest, sentinel *sentinelTokens, conduitToken string) (*fhttp.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	token, err := c.bearerToken(ctx)
	if err != nil {
		return nil, err
	}

	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodPost, baseURLChatGPT+chatGPTConversationPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	c.applyIdentityHeaders(req)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", baseURLChatGPT)
	req.Header.Set("Referer", baseURLChatGPT+"/")
	req.Header.Set("X-Conduit-Token", conduitToken)
	req.Header.Set("X-OpenAI-Target-Path", chatGPTConversationPath)
	req.Header.Set("X-OpenAI-Target-Route", chatGPTConversationPath)
	req.Header.Set("OpenAI-Sentinel-Chat-Requirements-Token", sentinel.RequirementsToken)
	if sentinel.ProofToken != "" {
		req.Header.Set("OpenAI-Sentinel-Proof-Token", sentinel.ProofToken)
	}
	if sentinel.TurnstileToken != "" {
		req.Header.Set("OpenAI-Sentinel-Turnstile-Token", sentinel.TurnstileToken)
	}
	if sentinel.SOToken != "" {
		req.Header.Set("OpenAI-Sentinel-SO-Token", sentinel.SOToken)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送消息失败: %w", err)
	}
	return resp, nil
}

// firstModelSlug 返回账号可用模型的第一个 slug（用于模型兜底）。
func (c *ChatGPTAPI) firstModelSlug(ctx context.Context) string {
	slugs, err := c.GetModels(ctx)
	if err != nil || len(slugs) == 0 {
		return ""
	}
	return slugs[0]
}

// applyIdentityHeaders 附加浏览器身份相关的请求头。
func (c *ChatGPTAPI) applyIdentityHeaders(req *fhttp.Request) {
	req.Header.Set("User-Agent", chromeUA)
	if c.cookieHdr != "" {
		req.Header.Set("Cookie", c.cookieHdr)
	}
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Priority", "u=1, i")
	req.Header.Set("Sec-Ch-Ua", `"Google Chrome";v="152", "Chromium";v="152", "Not_A Brand";v="24"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"macOS"`)
	req.Header.Set("Sec-Ch-Ua-Platform-Version", `"14.5.0"`)
	req.Header.Set("Sec-Ch-Ua-Arch", `"arm"`)
	req.Header.Set("Sec-Ch-Ua-Bitness", `"64"`)
	req.Header.Set("Sec-Ch-Ua-Full-Version", `"152.0.0.0"`)
	req.Header.Set("Sec-Ch-Ua-Full-Version-List", `"Google Chrome";v="152.0.0.0", "Chromium";v="152.0.0.0", "Not_A Brand";v="24.0.0.0"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("OAI-Device-Id", c.deviceID)
	req.Header.Set("OAI-Session-Id", c.oaiSessionID)
	req.Header.Set("OAI-Language", "zh-CN")
	req.Header.Set("OAI-Client-Version", chatGPTClientVersion)
	req.Header.Set("OAI-Client-Build-Number", chatGPTClientBuildNumber)
}

// --- SSE 解析 ---

// chatGPTSSEPayload 是 conversation 流的单个事件。
//
// 事件有四种形态：
//  1. 完整快照：message（含 v.message 包装）携带本条回复的累计全文
//  2. JSON-patch：p="/message/content/parts/0"，o=append/replace，v=增量文本
//  3. 纯增量：仅 v 为字符串
//  4. 控制事件：type（stream_handoff / message_stream_complete 等）
type chatGPTSSEPayload struct {
	Message        *chatGPTSSEMessage `json:"message"`
	V              json.RawMessage    `json:"v"`
	P              string             `json:"p"`
	O              string             `json:"o"`
	ConversationID string             `json:"conversation_id"`
	Type           string             `json:"type"`
	Error          string             `json:"error"`
}

// chatGPTSSEMessage 是 SSE 事件中的消息对象。
type chatGPTSSEMessage struct {
	ID     string `json:"id"`
	Author struct {
		Role string `json:"role"`
	} `json:"author"`
	Content *chatGPTSSEMessageContent `json:"content"`
	Status  string                    `json:"status"`
	EndTurn *bool                     `json:"end_turn"`
}

// chatGPTSSEMessageContent 是消息内容（parts 为累计文本片段）。
type chatGPTSSEMessageContent struct {
	ContentType string `json:"content_type"`
	Parts       []any  `json:"parts"`
	Text        string `json:"text"`
}

// sseMessage 提取事件中的 assistant 消息快照（兼容顶层 message 与 v.message 两种放置）。
func (p *chatGPTSSEPayload) sseMessage() *chatGPTSSEMessage {
	if p.Message != nil {
		return p.Message
	}
	if len(p.V) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(p.V)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var inner struct {
		Message *chatGPTSSEMessage `json:"message"`
	}
	if err := json.Unmarshal(trimmed, &inner); err != nil {
		return nil
	}
	return inner.Message
}

// sseDelta 提取增量文本（JSON-patch 的 append/replace 与无标记纯 delta）。
func (p *chatGPTSSEPayload) sseDelta() (string, bool) {
	if len(p.V) == 0 {
		return "", false
	}
	trimmed := bytes.TrimSpace(p.V)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", false
	}
	switch {
	case p.P == "/message/content/parts/0" && (p.O == "append" || p.O == "replace"):
		return s, true
	case p.P == "" && p.O == "":
		return s, true
	}
	return "", false
}

// parseSSE 解析 ChatGPT 的 SSE 流并转为增量事件。
func (c *ChatGPTAPI) parseSSE(ctx context.Context, r io.Reader, ch chan<- ChatGPTStreamChunk) {
	sseDebug := os.Getenv("OTTER_SSE_DEBUG") != ""
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	emit := func(chunk ChatGPTStreamChunk) bool {
		select {
		case ch <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}

	var convID, msgID, lastText string

	finish := func() {
		emit(ChatGPTStreamChunk{Type: "done", ConversationID: convID, MessageID: msgID})
	}

	// applyText 计算累计文本的增量并输出。
	applyText := func(text string) bool {
		if text == "" || text == lastText {
			return true
		}
		delta := text
		if strings.HasPrefix(text, lastText) {
			delta = text[len(lastText):]
		}
		lastText = text
		if delta == "" {
			return true
		}
		return emit(ChatGPTStreamChunk{Type: "text", Content: delta})
	}

	// handle 处理单个事件；返回 (是否正常结束, 是否中断)。
	var handle func(p *chatGPTSSEPayload) (done bool, stop bool)
	handle = func(p *chatGPTSSEPayload) (done bool, stop bool) {
		if p.Error != "" {
			emit(ChatGPTStreamChunk{Type: "error", Content: p.Error, Err: fmt.Errorf("ChatGPT 错误: %s", p.Error)})
			return false, true
		}
		if p.ConversationID != "" {
			convID = p.ConversationID
		}
		if p.Type == "stream_handoff" || p.Type == "resume_conversation_token" {
			return false, false
		}

		// 形态 1/2：完整消息快照（含 v.message 包装）
		if msg := p.sseMessage(); msg != nil && msg.Author.Role == "assistant" {
			// 仅正文类消息参与回复组装；model_editable_context 等元数据
			// 消息同样携带 finished_successfully，若参与判定会误判流提前结束
			if !isTextContentType(msg.Content) {
				return false, false
			}
			if msg.ID != "" && msg.ID != msgID {
				msgID = msg.ID
				lastText = ""
				if !emit(ChatGPTStreamChunk{Type: "meta", ConversationID: convID, MessageID: msgID}) {
					return false, true
				}
			}
			if !applyText(assistantMessageText(msg.Content)) {
				return false, true
			}
			// end_turn / finished_successfully 表示本条回复完成
			if (msg.EndTurn != nil && *msg.EndTurn) || msg.Status == "finished_successfully" {
				return true, false
			}
			return false, false
		}

		// v 为事件列表的 patch 包装：逐项递归处理
		if p.O == "patch" && len(p.V) > 0 {
			var items []json.RawMessage
			if json.Unmarshal(p.V, &items) == nil {
				for _, item := range items {
					var sub chatGPTSSEPayload
					if json.Unmarshal(item, &sub) != nil {
						continue
					}
					if d, s := handle(&sub); s || d {
						return d, s
					}
				}
			}
			return false, false
		}

		// 形态 3：增量事件（JSON-patch append/replace 或纯文本 delta）
		if delta, ok := p.sseDelta(); ok {
			text := lastText + delta
			if p.O == "replace" {
				text = delta
			}
			if sseDebug {
				fmt.Fprintf(os.Stderr, "[sse] delta=%q o=%q p=%q\n", delta, p.O, p.P)
			}
			if !applyText(text) {
				return false, true
			}
		}

		if p.Type == "message_stream_complete" {
			return true, false
		}
		return false, false
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			emit(ChatGPTStreamChunk{Type: "error", Err: ctx.Err()})
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" {
			continue
		}
		if sseDebug {
			fmt.Fprintf(os.Stderr, "[sse] %.320s\n", raw)
		}
		if raw == "[DONE]" {
			finish()
			return
		}

		// 数组包裹的批量事件：逐项处理
		if strings.HasPrefix(raw, "[") {
			var items []json.RawMessage
			if json.Unmarshal([]byte(raw), &items) != nil {
				continue
			}
			for _, item := range items {
				var sub chatGPTSSEPayload
				if json.Unmarshal(item, &sub) != nil {
					continue
				}
				done, stop := handle(&sub)
				if stop {
					return
				}
				if done {
					finish()
					return
				}
			}
			continue
		}

		var p chatGPTSSEPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			// 容错：跳过无法解析的帧（moderation 等无关事件）
			continue
		}
		done, stop := handle(&p)
		if stop {
			return
		}
		if done {
			finish()
			return
		}
	}

	if err := scanner.Err(); err != nil {
		emit(ChatGPTStreamChunk{Type: "error", Err: fmt.Errorf("读取 SSE 流失败: %w", err)})
		return
	}
	finish()
}

// isTextContentType 判断消息内容是否为正文文本类（参与回复组装）。
func isTextContentType(content *chatGPTSSEMessageContent) bool {
	if content == nil {
		return false
	}
	switch content.ContentType {
	case "", "text", "code", "multimodal_text":
		return true
	default:
		return false
	}
}

// assistantMessageText 提取 assistant 消息的文本（拼接 parts 中的字符串片段）。
//
// 仅接受文本类内容，跳过思考/执行输出等非正文片段。
func assistantMessageText(content *chatGPTSSEMessageContent) string {
	if !isTextContentType(content) {
		return ""
	}
	var sb strings.Builder
	for _, part := range content.Parts {
		if s, ok := part.(string); ok {
			sb.WriteString(s)
		}
	}
	if sb.Len() == 0 {
		return content.Text
	}
	return sb.String()
}

// GetSessionURL 返回 ChatGPT 会话页面 URL。
func (c *ChatGPTAPI) GetSessionURL(sessionID string) string {
	// ChatGPT conversation URL 格式：https://chatgpt.com/c/<conversation_id>
	if sessionID != "" {
		return fmt.Sprintf("https://chatgpt.com/c/%s", sessionID)
	}
	return "https://chatgpt.com/"
}

// drainBody 读取并关闭响应体（用于构建错误信息）。
func drainBody(resp *fhttp.Response) []byte {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return body
}

// truncateStr 截断过长字符串用于错误信息展示。
func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
