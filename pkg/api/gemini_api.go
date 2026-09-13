package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"

	"github.com/xiws/otter/pkg/chrome"
)

// Gemini Web 接口常量。
const (
	geminiBaseURL    = "https://gemini.google.com"
	geminiAppPath    = "/app"
	geminiStreamPath = "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate"
)

// geminiDefaultLanguage 是默认请求语言。
const geminiDefaultLanguage = "en"

// geminiStreamTimeout 是流式请求的整体超时上限。
const geminiStreamTimeout = 10 * time.Minute

// GeminiChunk 是 Gemini 流式响应中的一个事件。
type GeminiChunk struct {
	Type           string // "text" | "meta" | "done" | "error"
	Content        string
	Err            error
	ConversationID string
	ResponseID     string
	Metadata       []any // 多轮续聊所需的原始 metadata 数组
}

// GeminiAPI 封装 Gemini Web 的 StreamGenerate 直连协议。
type GeminiAPI struct {
	client    tls_client.HttpClient
	cookieHdr string
	at        string // SNlM0e：页面内嵌访问令牌
	bl        string // cfb2h：构建版本标签
	sid       string // f.sid：会话 ID
	language  string
	reqid     int
	mu        sync.Mutex
}

// NewGeminiAPI 创建一个新的 Gemini API 客户端。
func NewGeminiAPI() *GeminiAPI {
	client, err := newBrowserClient(geminiStreamTimeout)
	if err != nil {
		// newBrowserClient 仅在配置非法时失败，保留降级空间
		client = nil
	}
	return &GeminiAPI{
		client:   client,
		language: geminiDefaultLanguage,
		reqid:    int(time.Now().UnixNano()%90000) + 10000,
	}
}

// SetCookies 设置从 Chrome 提取的 cookie。
func (g *GeminiAPI) SetCookies(cookies []chrome.Cookie) {
	g.cookieHdr = chrome.CookieHeader(cookies)
}

// SetLanguage 设置请求语言（如 en / zh-CN）。
func (g *GeminiAPI) SetLanguage(lang string) {
	if lang != "" {
		g.language = lang
	}
}

// AccessToken 返回已提取的 SNlM0e 令牌（空串表示尚未初始化）。
func (g *GeminiAPI) AccessToken() string {
	return g.at
}

var (
	geminiAtRe  = regexp.MustCompile(`"SNlM0e":"([^"]*)"`)
	geminiBlRe  = regexp.MustCompile(`"cfb2h":"([^"]*)"`)
	geminiSidRe = regexp.MustCompile(`"FdrFJe":"([^"]*)"`)
)

// Init 打开 Gemini 页面并提取 SNlM0e / cfb2h / f.sid 令牌。
func (g *GeminiAPI) Init(ctx context.Context) error {
	if g.client == nil {
		return fmt.Errorf("Gemini HTTP 客户端初始化失败")
	}
	if g.cookieHdr == "" {
		return fmt.Errorf("Gemini cookie 未设置，请先执行 otter auth login gem")
	}

	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, geminiBaseURL+geminiAppPath, nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("Cookie", g.cookieHdr)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("打开 Gemini 页面失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return fmt.Errorf("读取 Gemini 页面失败: %w", err)
	}
	if resp.StatusCode != fhttp.StatusOK {
		return fmt.Errorf("打开 Gemini 页面失败: HTTP %d", resp.StatusCode)
	}

	html := string(body)
	at := firstMatch(geminiAtRe, html)
	bl := firstMatch(geminiBlRe, html)
	sid := firstMatch(geminiSidRe, html)
	if at == "" {
		return fmt.Errorf("未能从页面提取 SNlM0e 令牌（cookie 可能已过期，请在 Chrome 中重新登录 gemini.google.com 后执行 otter auth login gem）")
	}

	g.at = at
	g.bl = bl
	g.sid = sid
	return nil
}

// firstMatch 返回正则的第一个捕获组。
func firstMatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// geminiDefaultMetadata 是首轮对话的 metadata 模板。
func geminiDefaultMetadata() []any {
	return []any{"", "", "", nil, nil, nil, nil, nil, nil, ""}
}

// StreamGenerate 发送消息并返回流式事件。
//
// metadata 为上一轮响应的原始 metadata（多轮续聊必需），
// 首轮传 nil 使用默认模板。
func (g *GeminiAPI) StreamGenerate(ctx context.Context, prompt string, metadata []any) (<-chan GeminiChunk, error) {
	if g.client == nil {
		return nil, fmt.Errorf("Gemini HTTP 客户端初始化失败")
	}
	if g.at == "" {
		return nil, fmt.Errorf("Gemini 未初始化，请先调用 Init")
	}
	if metadata == nil {
		metadata = geminiDefaultMetadata()
	}

	// 构造 81 槽的 inner 请求体
	uuidVal := strings.ToUpper(newUUIDv4())
	inner := make([]any, 81)
	inner[0] = []any{prompt, 0, nil, nil, nil, nil, 0}
	inner[1] = []any{g.language}
	inner[2] = metadata
	inner[6] = []any{1}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []any{[]any{0}}
	inner[18] = 0
	inner[27] = 1
	inner[30] = []any{4}
	inner[41] = []any{1}
	inner[53] = 0
	inner[59] = uuidVal
	inner[61] = []any{}
	inner[68] = 1
	inner[79] = 1
	inner[80] = 1

	innerJSON, err := json.Marshal(inner)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	outerJSON, err := json.Marshal([]any{nil, string(innerJSON)})
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}

	g.mu.Lock()
	g.reqid += 100000
	reqid := g.reqid
	g.mu.Unlock()

	params := url.Values{}
	params.Set("hl", g.language)
	params.Set("_reqid", strconv.Itoa(reqid))
	params.Set("rt", "c")
	if g.bl != "" {
		params.Set("bl", g.bl)
	}
	if g.sid != "" {
		params.Set("f.sid", g.sid)
	}
	endpoint := geminiBaseURL + geminiStreamPath + "?" + params.Encode()

	form := url.Values{}
	form.Set("at", g.at)
	form.Set("f.req", string(outerJSON))

	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("Cookie", g.cookieHdr)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	req.Header.Set("Origin", geminiBaseURL)
	req.Header.Set("Referer", geminiBaseURL+"/")
	req.Header.Set("X-Same-Domain", "1")
	req.Header.Set("x-goog-ext-525005358-jspb", fmt.Sprintf(`["%s",1]`, uuidVal))

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("发送消息失败: %w", err)
	}
	if resp.StatusCode != fhttp.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("Gemini 返回 HTTP %d: %s", resp.StatusCode, string(body))
	}

	ch := make(chan GeminiChunk, 64)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		g.parseStream(ctx, resp.Body, ch)
	}()
	return ch, nil
}

// GetSessionURL 返回 Gemini 会话页面 URL。
func (g *GeminiAPI) GetSessionURL(cid string) string {
	if cid == "" {
		return geminiBaseURL + geminiAppPath
	}
	return geminiBaseURL + geminiAppPath + "/" + cid
}

// --- 流式帧解析 ---

// geminiStreamState 汇总解析过程中的会话状态。
type geminiStreamState struct {
	lastTexts   map[int]string
	lastCid     string
	lastRid     string
	lastMeta    []any
	completed   bool
	errored     bool
	sentText    bool
	framesParse int
}

// parseStream 解析 Gemini 的长度前缀流式帧。
//
// 格式：")]}'" 前缀 + 若干 "(\d+)\n<JSON>" 帧。
// 长度按 UTF-16 码元计算（astral 字符占 2），且计数从长度行后紧邻的
// 换行符开始、包含帧尾换行符（即 n = 正文码元数 + 2，已对真实流量验证）。
func (g *GeminiAPI) parseStream(ctx context.Context, r io.Reader, ch chan<- GeminiChunk) {
	st := &geminiStreamState{lastTexts: map[int]string{}}

	emit := func(c GeminiChunk) bool {
		select {
		case ch <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}

	reader := bufio.NewReaderSize(r, 64*1024)

	// 前缀：)]}'
	prefix := ")]}'"
	matched := 0
	for matched < len(prefix) {
		ru, _, err := reader.ReadRune()
		if err != nil {
			emit(GeminiChunk{Type: "error", Err: fmt.Errorf("读取响应流失败: %w", err)})
			return
		}
		if ru == '\uFEFF' || (matched == 0 && (ru == '\n' || ru == '\r' || ru == ' ')) {
			continue
		}
		if ru == rune(prefix[matched]) {
			matched++
			continue
		}
		emit(GeminiChunk{Type: "error", Err: fmt.Errorf("Gemini 响应格式异常（缺少前缀）")})
		return
	}

	const (
		stSep = iota
		stLen
		stPayload
	)
	state := stSep
	var lenBuf strings.Builder
	expectedUnits := -1
	payload := make([]rune, 0, 8192)
	payloadUnits := 0

	for {
		ru, _, err := reader.ReadRune()
		if err == io.EOF {
			break
		}
		if err != nil {
			emit(GeminiChunk{Type: "error", Err: fmt.Errorf("读取响应流失败: %w", err)})
			return
		}

		switch state {
		case stSep:
			if ru == '\n' || ru == '\r' || ru == ' ' || ru == '\t' {
				continue
			}
			state = stLen
			fallthrough
		case stLen:
			if ru >= '0' && ru <= '9' {
				lenBuf.WriteRune(ru)
				continue
			}
			if ru == '\r' {
				continue
			}
			if ru == '\n' {
				if lenBuf.Len() == 0 {
					continue
				}
				n, err := strconv.Atoi(lenBuf.String())
				if err != nil || n <= 0 {
					lenBuf.Reset()
					state = stSep
					continue
				}
				// 计数从本换行符开始并包含帧尾换行符，
				// 因此将本换行符作为载荷首字符计入
				expectedUnits = n
				payload = append(payload[:0], '\n')
				payloadUnits = 1
				lenBuf.Reset()
				state = stPayload
				continue
			}
			// 意外字符：重置长度解析
			lenBuf.Reset()
		case stPayload:
			payload = append(payload, ru)
			if ru > 0xFFFF {
				payloadUnits += 2
			} else {
				payloadUnits++
			}
			if payloadUnits >= expectedUnits {
				frame := string(payload)
				state = stSep
				expectedUnits = -1
				if !g.handleFrame(frame, st, emit) {
					return
				}
			}
		}
	}

	if st.errored {
		return
	}
	emit(GeminiChunk{Type: "done"})
}

// handleFrame 处理单个 JSON 帧；返回 false 表示应终止解析。
func (g *GeminiAPI) handleFrame(frame string, st *geminiStreamState, emit func(GeminiChunk) bool) bool {
	trimmed := strings.TrimSpace(frame)
	if trimmed == "" {
		return true
	}
	var items []any
	if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
		// 容错：跳过不可解析的帧
		return true
	}
	st.framesParse++

	for _, item := range items {
		arr, ok := item.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		kind, _ := arr[0].(string)
		if kind != "wrb.fr" || len(arr) < 3 {
			continue
		}
		innerStr, ok := arr[2].(string)
		if !ok || innerStr == "" {
			continue
		}
		if !g.handleInner(innerStr, st, emit) {
			return false
		}
	}
	return true
}

// handleInner 处理 wrb.fr 帧的内层 JSON；返回 false 表示终止。
func (g *GeminiAPI) handleInner(innerStr string, st *geminiStreamState, emit func(GeminiChunk) bool) bool {
	var inner []any
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
		return true
	}

	// 错误码：[5][2][0][1][0]
	if code, ok := dig(inner, 5, 2, 0, 1, 0).(float64); ok && code != 0 {
		st.errored = true
		err := fmt.Errorf("%s", geminiErrorMessage(int(code)))
		emit(GeminiChunk{Type: "error", Err: err})
		return false
	}

	// metadata：[1] = [cid, rid, ...]
	if metaArr, ok := dig(inner, 1).([]any); ok && len(metaArr) >= 2 {
		if cid, _ := metaArr[0].(string); cid != "" {
			st.lastCid = cid
		}
		if rid, _ := metaArr[1].(string); rid != "" {
			st.lastRid = rid
		}
		st.lastMeta = metaArr
		emit(GeminiChunk{
			Type:           "meta",
			ConversationID: st.lastCid,
			ResponseID:     st.lastRid,
			Metadata:       metaArr,
		})
	}

	// 收尾 context：[25] 为字符串时表示本轮写入历史，
	// metadata 更新为 [null x9, context]。
	if ctxStr, ok := dig(inner, 25).(string); ok && ctxStr != "" {
		final := make([]any, 10)
		final[9] = ctxStr
		st.lastMeta = final
		emit(GeminiChunk{
			Type:           "meta",
			ConversationID: st.lastCid,
			ResponseID:     st.lastRid,
			Metadata:       final,
		})
	}

	// candidates：[4] 数组
	candidates, ok := dig(inner, 4).([]any)
	if !ok {
		return true
	}
	for i, candAny := range candidates {
		cand, ok := candAny.([]any)
		if !ok {
			continue
		}
		if text, ok := dig(cand, 1, 0).(string); ok && text != "" {
			prev := st.lastTexts[i]
			var delta string
			if strings.HasPrefix(text, prev) {
				delta = text[len(prev):]
			} else {
				delta = text
			}
			st.lastTexts[i] = text
			if delta != "" {
				st.sentText = true
				if !emit(GeminiChunk{Type: "text", Content: delta}) {
					return false
				}
			}
		}
		if done, ok := dig(cand, 8, 0).(float64); ok && done == 2 {
			st.completed = true
		}
	}
	return true
}

// dig 沿着数组下标路径读取嵌套值，任一层不匹配返回 nil。
func dig(v any, path ...int) any {
	for _, p := range path {
		arr, ok := v.([]any)
		if !ok || p < 0 || p >= len(arr) {
			return nil
		}
		v = arr[p]
	}
	return v
}

// geminiErrorMessage 将 Gemini 错误码转换为可读信息。
func geminiErrorMessage(code int) string {
	switch code {
	case 1013:
		return "Gemini 临时错误（1013），请稍后重试"
	case 1037:
		return "Gemini 使用配额已用尽（1037），请稍后重试或切换账号"
	case 1050:
		return "会话模型不一致（1050），请执行 otter session new 开启新会话"
	case 1052:
		return "模型或请求结构已失效（1052），可能服务端已更新"
	case 1060:
		return "当前 IP 被 Google 临时限制（1060），请更换网络或稍后重试"
	default:
		return fmt.Sprintf("Gemini 返回错误码 %d", code)
	}
}
