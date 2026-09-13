// Package api 提供与各 AI 平台的 HTTP API 交互实现。
//
// # DeepSeek Direct API 实现
//
// chat.deepseek.com 的 Web 接口没有公开文档，以下端点均经实测确认：
//
//	POST /api/v0/users/login            账号密码登录
//	POST /api/v0/chat/create_pow_challenge  申请 PoW 挑战
//	POST /api/v0/chat_session/create    创建会话，返回 UUID
//	POST /api/v0/file/upload_file       上传附件，返回 file id
//	POST /api/v0/chat/completion        发送消息，返回 SSE 流
//
// 除登录外，其余写操作都要求请求头 x-ds-pow-response（见 pkg/pow）。
// 未知路径不会返回 404，而是回落到单页应用的 HTML，因此解析失败时
// 需要先确认端点是否写错。
package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiws/otter/pkg/pow"
)

// DeepSeek Web 接口路径。
const (
	baseURLDeepSeek   = "https://chat.deepseek.com"
	pathLogin         = "/api/v0/users/login"
	pathPowChallenge  = "/api/v0/chat/create_pow_challenge"
	pathSessionCreate = "/api/v0/chat_session/create"
	pathCompletion    = "/api/v0/chat/completion"
	pathFileUpload    = "/api/v0/file/upload_file"
	pathFileFetch     = "/api/v0/file/fetch_files"
	powHeaderName     = "x-ds-pow-response"
)

// DeepSeekAPI 封装 DeepSeek 聊天平台的 Direct API 调用。
type DeepSeekAPI struct {
	client    *Client
	authToken string
}

// NewDeepSeekAPI 创建一个新的 DeepSeek API 客户端。
func NewDeepSeekAPI() *DeepSeekAPI {
	return &DeepSeekAPI{
		client: NewClient(baseURLDeepSeek),
	}
}

// SetAuthToken 设置认证 token。
func (d *DeepSeekAPI) SetAuthToken(token string) {
	d.authToken = token
	d.client.SetAuthToken(token)
}

// GetAuthToken 返回当前的 auth token。
func (d *DeepSeekAPI) GetAuthToken() string {
	return d.authToken
}

// SetCookie 设置 Cookie（作为 auth token 的备选）。
func (d *DeepSeekAPI) SetCookie(cookie string) {
	d.client.SetCookie(cookie)
}

// SetTimeout 设置请求超时。设为 0 表示不限制（由 context 控制）。
func (d *DeepSeekAPI) SetTimeout(timeout time.Duration) {
	d.client.SetTimeout(timeout)
}

// --- 响应封装 ---

// envelope 是 DeepSeek 接口的统一外层结构。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// bizEnvelope 是 data 字段内的业务结构。
type bizEnvelope struct {
	BizCode int             `json:"biz_code"`
	BizMsg  string          `json:"biz_msg"`
	BizData json.RawMessage `json:"biz_data"`
}

// decodeEnvelope 校验两层状态码，返回 biz_data。
func decodeEnvelope(raw []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("接口返回错误: code=%d, msg=%s", env.Code, env.Msg)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, nil
	}

	var biz bizEnvelope
	_ = json.Unmarshal(env.Data, &biz)
	if biz.BizCode != 0 {
		return nil, fmt.Errorf("接口返回错误: %s", biz.BizMsg)
	}
	if len(biz.BizData) > 0 && string(biz.BizData) != "null" {
		return biz.BizData, nil
	}
	return env.Data, nil
}

// --- 登录 ---

// LoginUser 是登录成功返回的用户信息。
type LoginUser struct {
	ID     string `json:"id"`
	Token  string `json:"token"`
	Email  string `json:"email,omitempty"`
	Mobile string `json:"mobile,omitempty"`
}

// LoginRequest 是登录请求结构。
// email 与 mobile 二者只用其一：邮箱登录填 email，手机号登录填 mobile。
type LoginRequest struct {
	Email    string `json:"email"`
	Mobile   string `json:"mobile,omitempty"`
	Password string `json:"password"`
	AreaCode string `json:"area_code,omitempty"`
	DeviceID string `json:"device_id"`
	OS       string `json:"os"`
}

// splitAccount 按账号形态拆分登录标识：
// 含 "@" 视为邮箱，其余（手机号）视为手机号并去除空格、连字符与前导加号。
func splitAccount(account string) (email, mobile string) {
	account = strings.TrimSpace(account)
	if strings.Contains(account, "@") {
		return account, ""
	}
	return "", strings.NewReplacer(" ", "", "-", "", "+", "").Replace(account)
}

// Login 使用账号（邮箱或手机号）+密码登录 DeepSeek，返回并保存 auth_token。
func (d *DeepSeekAPI) Login(ctx context.Context, account, password string) (string, error) {
	deviceID := fmt.Sprintf("otter-cli-%x", time.Now().UnixNano())

	// DeepSeek 区分 email / mobile 两个字段：手机号登录时 email 必须留空，
	// 否则会返回 PASSWORD_OR_USER_NAME_IS_WRONG。
	email, mobile := splitAccount(account)

	reqBody := LoginRequest{
		Email:    email,
		Password: password,
		DeviceID: deviceID,
		OS:       "web",
	}
	if mobile != "" {
		reqBody.Mobile = mobile
		reqBody.AreaCode = "86"
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("编码登录请求失败: %w", err)
	}

	raw, err := d.client.PostRaw(ctx, pathLogin, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("登录请求失败: %w", err)
	}

	bizData, err := decodeEnvelope(raw)
	if err != nil {
		// 账号密码错误会以业务错误形式返回，这里补一句更友好的提示。
		if strings.Contains(err.Error(), "PASSWORD_OR_USER_NAME_IS_WRONG") {
			return "", fmt.Errorf("账号或密码错误（%w）", err)
		}
		return "", err
	}

	var payload struct {
		User *LoginUser `json:"user"`
	}
	if err := json.Unmarshal(bizData, &payload); err != nil {
		return "", fmt.Errorf("解析登录响应失败: %w", err)
	}
	if payload.User == nil || payload.User.Token == "" {
		return "", fmt.Errorf("登录成功但响应中未包含 token")
	}

	d.SetAuthToken(payload.User.Token)
	return payload.User.Token, nil
}

// --- PoW ---

// powHeader 为指定接口申请并求解 PoW 挑战，返回请求头值。
func (d *DeepSeekAPI) powHeader(ctx context.Context, targetPath string) (string, error) {
	body, err := json.Marshal(map[string]string{"target_path": targetPath})
	if err != nil {
		return "", fmt.Errorf("编码 PoW 请求失败: %w", err)
	}

	raw, err := d.client.PostRaw(ctx, pathPowChallenge, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("申请 PoW 挑战失败: %w", err)
	}

	bizData, err := decodeEnvelope(raw)
	if err != nil {
		return "", fmt.Errorf("申请 PoW 挑战失败: %w", err)
	}

	var resp struct {
		Challenge *pow.Challenge `json:"challenge"`
	}
	if err := json.Unmarshal(bizData, &resp); err != nil {
		return "", fmt.Errorf("解析 PoW 挑战失败: %w", err)
	}
	if resp.Challenge == nil {
		return "", fmt.Errorf("PoW 挑战为空")
	}

	header, err := resp.Challenge.SolveHeader(ctx)
	if err != nil {
		return "", fmt.Errorf("求解 PoW 失败: %w", err)
	}
	return header, nil
}

// --- 会话 ---

// CreateSession 在 DeepSeek 平台创建新会话，返回会话 UUID。
func (d *DeepSeekAPI) CreateSession(ctx context.Context) (string, error) {
	raw, err := d.client.PostRaw(ctx, pathSessionCreate, "application/json", strings.NewReader("{}"))
	if err != nil {
		return "", fmt.Errorf("创建会话失败: %w", err)
	}

	bizData, err := decodeEnvelope(raw)
	if err != nil {
		return "", fmt.Errorf("创建会话失败: %w", err)
	}

	var sess struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(bizData, &sess); err != nil {
		return "", fmt.Errorf("解析会话响应失败: %w", err)
	}
	if sess.ID == "" {
		return "", fmt.Errorf("创建会话未返回会话 ID")
	}
	return sess.ID, nil
}

// --- 文件上传 ---

// FileUploadResponse 是文件上传的结果。
type FileUploadResponse struct {
	ID       string `json:"id"`
	FileName string `json:"file_name"`
	FileSize int64  `json:"file_size"`
	Status   string `json:"status"`
}

// FileInfo 是平台侧的文件信息。
type FileInfo struct {
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	FileName   string          `json:"file_name"`
	FileSize   int64           `json:"file_size"`
	TokenUsage json.RawMessage `json:"token_usage"`
	ErrorCode  json.RawMessage `json:"error_code"`
}

// FetchFiles 查询一组文件在平台侧的处理状态。
func (d *DeepSeekAPI) FetchFiles(ctx context.Context, fileIDs []string) ([]FileInfo, error) {
	q := url.Values{}
	for _, id := range fileIDs {
		q.Add("file_ids", id)
	}

	var raw json.RawMessage
	if err := d.client.GetJSON(ctx, pathFileFetch+"?"+q.Encode(), &raw); err != nil {
		return nil, fmt.Errorf("查询文件状态失败: %w", err)
	}
	bizData, err := decodeEnvelope(raw)
	if err != nil {
		return nil, fmt.Errorf("查询文件状态失败: %w", err)
	}

	var resp struct {
		Files []FileInfo `json:"files"`
	}
	if err := json.Unmarshal(bizData, &resp); err != nil {
		return nil, fmt.Errorf("解析文件状态失败: %w", err)
	}
	return resp.Files, nil
}

// WaitForFileReady 轮询文件状态，直到平台处理完成。
//
// DeepSeek 的上传是异步的：刚上传的文件处于 PENDING，此时把它当
// ref_file_ids 引用会被拒（biz_code=9 invalid ref file id）。
func (d *DeepSeekAPI) WaitForFileReady(ctx context.Context, fileID string) error {
	const (
		interval = 500 * time.Millisecond
		timeout  = 60 * time.Second
	)
	deadline := time.Now().Add(timeout)

	for {
		files, err := d.FetchFiles(ctx, []string{fileID})
		if err != nil {
			return err
		}
		for _, f := range files {
			if f.ID != fileID {
				continue
			}
			switch strings.ToUpper(f.Status) {
			case "SUCCESS":
				return nil
			case "FAILED", "ERROR":
				return fmt.Errorf("文件 %s 处理失败（status=%s）", fileID, f.Status)
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("等待文件 %s 处理超时", fileID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// UploadFile 上传文件到 DeepSeek，返回 file id（形如 file-xxxx）。
//
// 返回时文件已处理就绪，可直接作为 ref_file_ids 引用。
func (d *DeepSeekAPI) UploadFile(ctx context.Context, filePath string) (*FileUploadResponse, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("获取文件绝对路径失败: %w", err)
	}

	file, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("file", filepath.Base(absPath))
	if err != nil {
		return nil, fmt.Errorf("创建 form 字段失败: %w", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return nil, fmt.Errorf("写入文件数据失败: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("构造上传请求失败: %w", err)
	}

	header, err := d.powHeader(ctx, pathFileUpload)
	if err != nil {
		return nil, err
	}

	raw, err := d.client.PostRawWithHeaders(ctx, pathFileUpload, writer.FormDataContentType(), &buf,
		map[string]string{powHeaderName: header})
	if err != nil {
		return nil, fmt.Errorf("上传文件失败: %w", err)
	}

	bizData, err := decodeEnvelope(raw)
	if err != nil {
		return nil, fmt.Errorf("上传文件失败: %w", err)
	}

	var resp FileUploadResponse
	if err := json.Unmarshal(bizData, &resp); err != nil {
		return nil, fmt.Errorf("解析上传响应失败: %w", err)
	}
	if resp.ID == "" {
		return nil, fmt.Errorf("上传成功但未返回文件 ID")
	}

	// 上传是异步的，等平台处理完再返回，避免调用方拿到不可用的 file id。
	if err := d.WaitForFileReady(ctx, resp.ID); err != nil {
		return nil, err
	}
	return &resp, nil
}

// --- 聊天 ---

// CompletionRequest 是一次对话请求的参数。
type CompletionRequest struct {
	// ChatSessionID 是平台侧的会话 UUID（由 CreateSession 获得）。
	ChatSessionID string
	// ParentMessageID 是多轮对话的上一条回复 ID；首轮传 0，请求中会发送 null。
	ParentMessageID int
	Prompt          string
	FileIDs         []string
	ThinkingEnabled bool
	SearchEnabled   bool
}

// StreamChunk 是流式响应中的一个事件。
type StreamChunk struct {
	// Type 取值：text（正文增量）、meta（元信息）、done（结束）、error。
	Type string
	// Content 仅在 Type 为 text / error 时有意义。
	Content string
	// ResponseMessageID 是本轮回复的消息 ID，下次追问需作为 parent_message_id。
	ResponseMessageID int
	// TokenUsage 是本轮累计 token 用量。
	TokenUsage int
	// Title 是平台自动生成的会话标题。
	Title string
}

// Completion 发送消息并以流式方式返回响应。
//
// 该方法不会重试：重试会导致重复发送消息。
func (d *DeepSeekAPI) Completion(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	if d.authToken == "" {
		return nil, fmt.Errorf("未认证，请先登录 DeepSeek")
	}
	if req.ChatSessionID == "" {
		return nil, fmt.Errorf("缺少会话 ID")
	}

	header, err := d.powHeader(ctx, pathCompletion)
	if err != nil {
		return nil, err
	}

	var parent any
	if req.ParentMessageID > 0 {
		parent = req.ParentMessageID
	}
	fileIDs := req.FileIDs
	if fileIDs == nil {
		fileIDs = []string{}
	}

	payload := map[string]any{
		"chat_session_id":   req.ChatSessionID,
		"parent_message_id": parent,
		"prompt":            req.Prompt,
		"ref_file_ids":      fileIDs,
		"thinking_enabled":  req.ThinkingEnabled,
		"search_enabled":    req.SearchEnabled,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("编码请求体失败: %w", err)
	}

	// 流式响应可能持续较久，交给 context 控制超时，不使用固定 HTTP 超时。
	d.client.SetTimeout(0)

	resp, err := d.client.DoOnce(ctx, http.MethodPost, pathCompletion, bytes.NewReader(body),
		map[string]string{
			"Content-Type": "application/json",
			powHeaderName:  header,
		})
	if err != nil {
		return nil, fmt.Errorf("发送消息失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("发送消息失败: HTTP %d: %s", resp.StatusCode, string(detail))
	}

	ch := make(chan StreamChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		parseSSE(ctx, resp.Body, ch)
	}()
	return ch, nil
}

// ssePayload 是 SSE 中 data 行的通用结构。
// 不同事件复用同一批字段：正文增量带 p/v，会话标题只带 content；
// 接口在 HTTP 200 下也可能返回业务错误信封（code/biz_code）。
type ssePayload struct {
	V                 json.RawMessage `json:"v"`
	P                 string          `json:"p"`
	O                 string          `json:"o"`
	Content           string          `json:"content"`
	RequestMessageID  int             `json:"request_message_id"`
	ResponseMessageID int             `json:"response_message_id"`

	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data *struct {
		BizCode int    `json:"biz_code"`
		BizMsg  string `json:"biz_msg"`
	} `json:"data"`
}

// bizError 判断该帧是否为业务错误信封，并返回错误描述。
func (p *ssePayload) bizError() (string, bool) {
	if p.Data == nil {
		return "", false
	}
	if p.Data.BizCode != 0 {
		msg := p.Data.BizMsg
		if msg == "" {
			msg = fmt.Sprintf("biz_code=%d", p.Data.BizCode)
		}
		return msg, true
	}
	if p.Code != 0 && p.Msg != "" {
		return p.Msg, true
	}
	return "", false
}

// parseSSE 解析 DeepSeek 的 text/event-stream 响应。
//
// 正文增量有两种形态，都必须处理：
//
//	data: {"p":"response/content","o":"APPEND","v":"一二"}
//	data: {"v":"三四"}
func parseSSE(ctx context.Context, r io.Reader, ch chan<- StreamChunk) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// 状态帧（FINISHED）之后服务端还会发会话标题等收尾事件，
	// 因此不能在那里直接返回，只标记 done 已发出。
	doneSent := false

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			ch <- StreamChunk{Type: "error", Content: ctx.Err().Error()}
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "{}" {
			continue
		}

		var p ssePayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			continue
		}

		// HTTP 200 下的业务错误（如 invalid ref file id）必须报出来，
		// 否则调用方只会看到空回复。
		if msg, isErr := p.bizError(); isErr {
			ch <- StreamChunk{Type: "error", Content: msg}
			return
		}

		// 首帧：{"request_message_id":1,"response_message_id":2,...}
		if p.ResponseMessageID != 0 {
			ch <- StreamChunk{Type: "meta", ResponseMessageID: p.ResponseMessageID}
			continue
		}
		// 标题事件：{"content":"..."}
		if p.Content != "" && len(p.V) == 0 {
			ch <- StreamChunk{Type: "meta", Title: p.Content}
			continue
		}
		if len(p.V) == 0 {
			continue
		}

		// v 为字符串：正文增量或状态
		var s string
		if err := json.Unmarshal(p.V, &s); err == nil {
			switch p.P {
			case "", "response/content":
				if s != "" {
					ch <- StreamChunk{Type: "text", Content: s}
				}
			case "response/status":
				if s == "FINISHED" && !doneSent {
					ch <- StreamChunk{Type: "done"}
					doneSent = true
				}
			}
			continue
		}

		// v 为数字：token 用量
		var n int
		if err := json.Unmarshal(p.V, &n); err == nil {
			if p.P == "response/accumulated_token_usage" {
				ch <- StreamChunk{Type: "meta", TokenUsage: n}
			}
			continue
		}

		// v 为对象：初始 response 帧，取出 message_id 与内容快照。
		//
		// 服务端在这里给出“当前已累积的内容”：长回复该字段为空，
		// 内容随后续增量帧到达；而短回复（服务端一次生成完）
		// 不再发增量帧，内容只存在这个快照里，必须取出，
		// 否则整条回复会丢失。
		var obj struct {
			Response struct {
				MessageID int    `json:"message_id"`
				Content   string `json:"content"`
			} `json:"response"`
		}
		if err := json.Unmarshal(p.V, &obj); err == nil && obj.Response.MessageID != 0 {
			ch <- StreamChunk{Type: "meta", ResponseMessageID: obj.Response.MessageID}
			if obj.Response.Content != "" {
				ch <- StreamChunk{Type: "text", Content: obj.Response.Content}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		ch <- StreamChunk{Type: "error", Content: fmt.Sprintf("读取响应流失败: %v", err)}
		return
	}
	if !doneSent {
		ch <- StreamChunk{Type: "done"}
	}
}

// GetSessionURL 返回 DeepSeek 会话页面 URL。
func (d *DeepSeekAPI) GetSessionURL(sessionID string) string {
	return fmt.Sprintf("https://chat.deepseek.com/a/chat/s/%s", sessionID)
}
