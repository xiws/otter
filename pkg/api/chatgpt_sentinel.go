// Package api 提供与各 AI 平台的 HTTP API 交互实现。
package api

// 本文件实现 ChatGPT 的 sentinel 反爬协议（chat-requirements 流程）。
//
// 流程：
//  1. 生成 requirements token（p）："gAAAAAC" + base64(JSON(浏览器环境配置数组))
//  2. POST /backend-api/sentinel/chat-requirements/prepare 获取 PoW 挑战
//  3. 求解 SHA3-512 工作量证明（proof token："gAAAAAB" + base64）
//  4. POST /backend-api/sentinel/chat-requirements/finalize 换取正式 token
//
// 最终令牌用于 /backend-api/f/conversation 的 OpenAI-Sentinel-* 请求头。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"golang.org/x/crypto/sha3"
)

// ChatGPT sentinel 协议常量。
const (
	chatGPTSentinelPreparePath  = "/backend-api/sentinel/chat-requirements/prepare"
	chatGPTSentinelFinalizePath = "/backend-api/sentinel/chat-requirements/finalize"
	chatGPTSentinelSDKURL       = baseURLChatGPT + "/backend-api/sentinel/sdk.js"

	// chatGPTPoWMaxAttempts 是工作量证明的最大迭代次数（对齐浏览器 SDK）。
	chatGPTPoWMaxAttempts = 500000
)

// sentinelTokens 是一次 sentinel 握手的结果，供 conversation 请求头使用。
type sentinelTokens struct {
	RequirementsToken string // OpenAI-Sentinel-Chat-Requirements-Token
	ProofToken        string // OpenAI-Sentinel-Proof-Token
	TurnstileToken    string // OpenAI-Sentinel-Turnstile-Token（可能为空）
	SOToken           string // OpenAI-Sentinel-SO-Token（可能为空）

	// 以下字段仅供调试对照使用（OTTER_SENTINEL_DUMP）。
	pToken        string
	powSeed       string
	powDifficulty string
}

// sentinelPrepareResponse 是 chat-requirements/prepare 的响应结构。
type sentinelPrepareResponse struct {
	PrepareToken string `json:"prepare_token"`
	ProofOfWork  *struct {
		Required   bool   `json:"required"`
		Seed       string `json:"seed"`
		Difficulty string `json:"difficulty"`
	} `json:"proofofwork"`
	Turnstile *struct {
		Required bool   `json:"required"`
		Dx       string `json:"dx"`
	} `json:"turnstile"`
	Arkose *struct {
		Required bool `json:"required"`
	} `json:"arkose"`
}

// sentinelNavigatorKeys 模拟 navigator 原型属性探测结果（原型 − 值 格式）。
var sentinelNavigatorKeys = []string{
	"registerProtocolHandler−function registerProtocolHandler() { [native code] }",
	"storage−[object StorageManager]",
	"locks−[object LockManager]",
	"appCodeName−Mozilla",
	"permissions−[object Permissions]",
	"share−function share() { [native code] }",
	"webdriver−false",
	"managed−[object NavigatorManagedData]",
	"canShare−function canShare() { [native code] }",
	"vendor−Google Inc.",
	"mediaDevices−[object MediaDevices]",
	"vibrate−function vibrate() { [native code] }",
	"storageBuckets−[object StorageBucketManager]",
	"mediaCapabilities−[object MediaCapabilities]",
	"cookieEnabled−true",
	"virtualKeyboard−[object VirtualKeyboard]",
	"product−Gecko",
	"presentation−[object Presentation]",
	"onLine−true",
	"mimeTypes−[object MimeTypeArray]",
	"credentials−[object CredentialsContainer]",
	"serviceWorker−[object ServiceWorkerContainer]",
	"keyboard−[object Keyboard]",
	"gpu−[object GPU]",
	"doNotTrack",
	"serial−[object Serial]",
	"pdfViewerEnabled−true",
	"language−zh-CN",
	"geolocation−[object Geolocation]",
	"userAgentData−[object NavigatorUAData]",
	"getUserMedia−function getUserMedia() { [native code] }",
	"sendBeacon−function sendBeacon() { [native code] }",
	"hardwareConcurrency−32",
	"windowControlsOverlay−[object WindowControlsOverlay]",
}

// sentinelWindowKeys 模拟 window 对象随机键名探测。
var sentinelWindowKeys = []string{
	"0", "window", "self", "document", "name", "location",
	"customElements", "history", "navigation", "innerWidth", "innerHeight",
	"scrollX", "scrollY", "visualViewport", "screenX", "screenY",
	"outerWidth", "outerHeight", "devicePixelRatio", "screen", "chrome",
	"navigator", "onresize", "performance", "crypto", "indexedDB",
	"sessionStorage", "localStorage", "scheduler", "alert", "atob", "btoa",
	"fetch", "matchMedia", "postMessage", "queueMicrotask",
	"requestAnimationFrame", "setInterval", "setTimeout", "caches",
	"__NEXT_DATA__", "__BUILD_MANIFEST", "__NEXT_PRELOADREADY",
}

// sentinelDocumentKeys 模拟 document 对象随机键名探测。
var sentinelDocumentKeys = []string{
	"__reactContainer$fzelfjyxej8",
	"_reactListening5dehydibo78",
	"location",
}

// sentinelScreens 与 sentinelCores 是常见的屏幕尺寸 / CPU 核心数取值池。
var (
	sentinelScreens = [][2]int{{1920, 1080}, {1440, 900}, {2560, 1440}, {3840, 2160}}
	sentinelCores   = []int{8, 16, 24, 32}
)

// poWConfig 构造 sentinel 工作量证明的环境配置数组（25 元素）。
//
// 数组语义与浏览器 SDK getConfig() 一致；config[3]、config[9] 会在
// 求解过程中被替换为迭代计数器，此处先填充占位值。
func (c *ChatGPTAPI) poWConfig(scriptSources []string, dataBuild string) []any {
	now := time.Now().In(time.FixedZone("EST", -5*3600))
	dateStr := now.Format("Mon Jan 02 2006 15:04:05") + " GMT-0500 (Eastern Standard Time)"
	perfNow := 1000 + rand.Float64()*49000
	timeOrigin := float64(time.Now().UnixMilli()) - perfNow
	var script any
	if len(scriptSources) > 0 {
		script = scriptSources[rand.Intn(len(scriptSources))]
	}
	screen := sentinelScreens[rand.Intn(len(sentinelScreens))]
	return []any{
		screen[0] + screen[1],
		dateStr,
		4294705152,
		1,
		chromeUA,
		script,
		dataBuild,
		"en-US",
		"en-US,es-US,en,es",
		rand.Float64(),
		sentinelNavigatorKeys[rand.Intn(len(sentinelNavigatorKeys))],
		sentinelDocumentKeys[rand.Intn(len(sentinelDocumentKeys))],
		sentinelWindowKeys[rand.Intn(len(sentinelWindowKeys))],
		perfNow,
		newUUIDv4(),
		"",
		sentinelCores[rand.Intn(len(sentinelCores))],
		timeOrigin,
		0, 0, 0, 0, 0, 0, 0,
	}
}

// solvePoW 求解工作量证明。
//
// 对迭代计数 i 构造 base64(JSON(config))（其中 config[3]=i、config[9]=i>>1），
// 使 SHA3-512(seed + answer) 的前缀字节不超过难度目标；成功返回 answer。
func solvePoW(seed, difficulty string, config []any) (string, bool) {
	target, err := hex.DecodeString(difficulty)
	if err != nil || len(target) == 0 {
		return "", false
	}
	diffLen := len(difficulty) / 2
	if diffLen > len(target) {
		return "", false
	}
	seedBytes := []byte(seed)

	// 预拼接 config 的静态片段，循环内只替换两个计数器
	part1 := marshalJSON(config[:3])
	part2 := marshalJSON(config[4:9])
	part3 := marshalJSON(config[10:])
	if len(part1) < 2 || len(part2) < 2 || len(part3) < 2 {
		return "", false
	}
	static1 := string(part1[:len(part1)-1]) + ","
	static2 := "," + string(part2[1:len(part2)-1]) + ","
	static3 := "," + string(part3[1:])

	hasher := sha3.New512()
	for i := 0; i < chatGPTPoWMaxAttempts; i++ {
		payload := static1 + strconv.Itoa(i) + static2 + strconv.Itoa(i>>1) + static3
		answer := base64.StdEncoding.EncodeToString([]byte(payload))
		hasher.Reset()
		hasher.Write(seedBytes)
		hasher.Write([]byte(answer))
		if bytes.Compare(hasher.Sum(nil)[:diffLen], target) <= 0 {
			return answer, true
		}
	}
	return "", false
}

// marshalJSON 序列化 JSON 且不转义 HTML 字符（对齐浏览器 JSON.stringify 语义）。
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		raw, _ := json.Marshal(v)
		return raw
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// 首页资源提取正则。
var (
	sentinelScriptSrcRe = regexp.MustCompile(`<script[^>]+src="([^"]+)"`)
	sentinelBuildPathRe = regexp.MustCompile(`c/[^/]*/_`)
	sentinelDataBuildRe = regexp.MustCompile(`data-build="([^"]*)"`)
)

// sentinelResources 返回首页脚本列表与 build 标识（bootstrap 仅执行一次）。
func (c *ChatGPTAPI) sentinelResources(ctx context.Context) ([]string, string) {
	c.sentinelOnce.Do(func() {
		c.sentinelScripts, c.sentinelBuild = c.fetchSentinelResources(ctx)
	})
	return c.sentinelScripts, c.sentinelBuild
}

// fetchSentinelResources 拉取 ChatGPT 首页并提取脚本 src 与 build 标识。
//
// 失败时退回默认 SDK 地址；bootstrap 结果仅用于让 PoW 配置更接近真实浏览器。
func (c *ChatGPTAPI) fetchSentinelResources(ctx context.Context) ([]string, string) {
	debug := os.Getenv("OTTER_TURNSTILE_DEBUG") != ""
	fallback := []string{chatGPTSentinelSDKURL}
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, baseURLChatGPT+"/", nil)
	if err != nil {
		return fallback, ""
	}
	c.applyIdentityHeaders(req)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")

	resp, err := c.client.Do(req)
	if err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[bootstrap] 请求失败: %v\n", err)
		}
		return fallback, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != fhttp.StatusOK {
		if debug {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			fmt.Fprintf(os.Stderr, "[bootstrap] HTTP %d: %.200s\n", resp.StatusCode, body)
		}
		return fallback, ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fallback, ""
	}
	html := string(body)
	scripts := make([]string, 0, 16)
	for _, m := range sentinelScriptSrcRe.FindAllStringSubmatch(html, -1) {
		if len(m) > 1 && m[1] != "" {
			scripts = append(scripts, m[1])
		}
	}
	// build 标识与脚本列表独立提取：优先脚本路径中的 c/<hash>/_ 片段，
	// 否则退回 html 的 data-build 属性（真实浏览器 getConfig() 的取值顺序）。
	build := sentinelBuildPathRe.FindString(html)
	if build == "" {
		if m := sentinelDataBuildRe.FindStringSubmatch(html); len(m) > 1 {
			build = m[1]
		}
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[bootstrap] html=%dB scripts=%d build=%q\n", len(html), len(scripts), build)
	}
	if len(scripts) == 0 {
		return fallback, build
	}
	return scripts, build
}

// fetchSentinel 执行完整的 sentinel 握手，返回 conversation 需要的令牌集合。
func (c *ChatGPTAPI) fetchSentinel(ctx context.Context) (*sentinelTokens, error) {
	scripts, dataBuild := c.sentinelResources(ctx)

	// 1. requirements token（p）
	pToken := "gAAAAAC" + base64.StdEncoding.EncodeToString(marshalJSON(c.poWConfig(scripts, dataBuild)))

	// 2. prepare
	var prepare sentinelPrepareResponse
	if err := c.postSentinelJSON(ctx, chatGPTSentinelPreparePath, map[string]string{"p": pToken}, &prepare); err != nil {
		return nil, fmt.Errorf("sentinel prepare 失败: %w", err)
	}
	if prepare.PrepareToken == "" {
		return nil, fmt.Errorf("sentinel prepare 响应缺少 prepare_token")
	}
	if prepare.Arkose != nil && prepare.Arkose.Required {
		return nil, fmt.Errorf("sentinel 要求 arkose 验证（暂不支持）")
	}

	// 3. 求解 PoW
	proofToken := ""
	powSeed, powDifficulty := "", ""
	if prepare.ProofOfWork != nil && prepare.ProofOfWork.Required {
		powSeed = prepare.ProofOfWork.Seed
		powDifficulty = prepare.ProofOfWork.Difficulty
		answer, ok := solvePoW(powSeed, powDifficulty, c.poWConfig(scripts, dataBuild))
		if !ok {
			return nil, fmt.Errorf("sentinel PoW 求解失败（difficulty=%s）", prepare.ProofOfWork.Difficulty)
		}
		proofToken = "gAAAAAB" + answer
	}

	// 4. turnstile（需要执行 dx 指令虚拟机）
	//
	// 参考实现的容错行为：求解失败时以空 token 继续 finalize（服务端可接受）。
	// 这里额外丢弃明显异常的结果（真实 token 为长 base64，过短的属于执行岔路）。
	turnstileToken := ""
	if prepare.Turnstile != nil && prepare.Turnstile.Required {
		if prepare.Turnstile.Dx == "" {
			return nil, fmt.Errorf("sentinel 要求 turnstile 验证但未提供 dx")
		}
		if solved, ok := solveTurnstileToken(prepare.Turnstile.Dx, pToken); ok && looksLikeTurnstileToken(solved) {
			turnstileToken = solved
		} else if os.Getenv("OTTER_TURNSTILE_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "[turnstile] 求解结果不可用，改用空 token 继续（solved=%q）\n", solved)
		}
	}

	// 5. finalize
	var finalize struct {
		Token   string `json:"token"`
		SOToken string `json:"so_token"`
	}
	body := map[string]string{
		"prepare_token":   prepare.PrepareToken,
		"proof_token":     proofToken,
		"turnstile_token": turnstileToken,
	}
	if err := c.postSentinelJSON(ctx, chatGPTSentinelFinalizePath, body, &finalize); err != nil {
		return nil, fmt.Errorf("sentinel finalize 失败: %w", err)
	}
	if finalize.Token == "" {
		return nil, fmt.Errorf("sentinel finalize 响应缺少 token")
	}

	return &sentinelTokens{
		RequirementsToken: finalize.Token,
		ProofToken:        proofToken,
		TurnstileToken:    turnstileToken,
		SOToken:           finalize.SOToken,
		pToken:            pToken,
		powSeed:           powSeed,
		powDifficulty:     powDifficulty,
	}, nil
}

// postSentinelJSON 向 sentinel 端点发送 JSON 请求并解析响应。
func (c *ChatGPTAPI) postSentinelJSON(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodPost, baseURLChatGPT+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	c.applyIdentityHeaders(req)
	// sentinel 握手必须与后续 conversation 使用同一认证上下文：
	// 浏览器在已登录状态下所有 sentinel 请求均携带 Bearer token。
	if token, err := c.bearerToken(ctx); err == nil && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Origin", baseURLChatGPT)
	req.Header.Set("Referer", baseURLChatGPT+"/")
	req.Header.Set("X-OpenAI-Target-Path", path)
	req.Header.Set("X-OpenAI-Target-Route", path)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode != fhttp.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(data), 300))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("解析响应失败: %w: %s", err, truncateStr(strings.TrimSpace(string(data)), 200))
		}
	}
	return nil
}

// --- turnstile dx 指令虚拟机 ---
//
// prepare 响应在 turnstile.required 时附带 dx：base64(XOR(JSON 指令队列))，
// XOR 密钥为本次的 requirements token（p）。指令队列在微型虚拟机中执行
// （模拟浏览器属性探测），最终由输出指令产出 turnstile token。

// tsCallable 是虚拟机中可调用的内置过程。
type tsCallable func(args ...any) any

// tsOrderedMap 模拟 JS Map（保持插入顺序）。
type tsOrderedMap struct {
	keys []string
	vals map[string]any
}

// add 写入键值（首次插入记录顺序）。
func (m *tsOrderedMap) add(key string, v any) {
	if _, ok := m.vals[key]; !ok {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = v
}

// tsVM 是 turnstile 指令队列的执行状态。
type tsVM struct {
	vals  map[float64]any
	start time.Time
	out   string
}

// turnstileStrSpecials 是属性探测字符串到浏览器表现值的映射。
var turnstileStrSpecials = map[string]string{
	"window.Math":            "[object Math]",
	"window.Reflect":         "[object Reflect]",
	"window.performance":     "[object Performance]",
	"window.localStorage":    "[object Storage]",
	"window.Object":          "function Object() { [native code] }",
	"window.Reflect.set":     "function set() { [native code] }",
	"window.performance.now": "function () { [native code] }",
	"window.Object.create":   "function create() { [native code] }",
	"window.Object.keys":     "function keys() { [native code] }",
	"window.Math.random":     "function random() { [native code] }",
}

// turnstileToStr 按虚拟机语义将值转为字符串。
func turnstileToStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case string:
		if s, ok := turnstileStrSpecials[x]; ok {
			return s
		}
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			s, ok := item.(string)
			if !ok {
				return fmt.Sprint(x)
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(v)
}

// toFloat 将参数转为虚拟机槽位索引。
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

// argKey 取第 i 个参数作为槽位索引。
func argKey(args []any, i int) (float64, bool) {
	if i >= len(args) {
		return 0, false
	}
	return toFloat(args[i])
}

// argValue 取第 i 个参数对应的槽位值（参数不是索引时返回 nil）。
func (vm *tsVM) argValue(args []any, i int) any {
	idx, ok := argKey(args, i)
	if !ok {
		return nil
	}
	return vm.vals[idx]
}

// xorRunes 按 rune 循环异或 a 与密钥 b。
func xorRunes(a, b string) string {
	if b == "" {
		return a
	}
	br := []rune(b)
	ar := []rune(a)
	out := make([]rune, len(ar))
	for i, ch := range ar {
		out[i] = ch ^ br[i%len(br)]
	}
	return string(out)
}

// isStrOrNum 判断值是否为字符串或数字。
func isStrOrNum(v any) bool {
	switch v.(type) {
	case string, json.Number, float64, int:
		return true
	}
	return false
}

// looksLikeTurnstileToken 粗略判断 dx 求解结果是否为可用的 turnstile token。
//
// 正常 token 是较长 base64 字符串；虚拟机执行岔路产生的残缺值（如 "OH"）应当丢弃。
func looksLikeTurnstileToken(token string) bool {
	if len(token) < 100 {
		return false
	}
	for _, r := range token {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// solveTurnstileToken 解码并执行 dx 指令队列，返回 turnstile token。
func solveTurnstileToken(dx, p string) (string, bool) {
	debug := os.Getenv("OTTER_TURNSTILE_DEBUG") != ""
	decoded, err := base64.StdEncoding.DecodeString(dx)
	if err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[turnstile] base64 解码失败: %v (dx head: %.60s)\n", err, dx)
		}
		return "", false
	}
	plain := xorRunes(string(decoded), p)
	if debug {
		fmt.Fprintf(os.Stderr, "[turnstile] dx=%dB decoded=%dB plain=%dB p=%dB\n", len(dx), len(decoded), len(plain), len(p))
		fmt.Fprintf(os.Stderr, "[turnstile] plain head: %.200s\n", plain)
	}

	dec := json.NewDecoder(strings.NewReader(plain))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		if debug {
			fmt.Fprintf(os.Stderr, "[turnstile] JSON 解析失败: %v\n", err)
		}
		return "", false
	}
	tokenList, ok := parsed.([]any)
	if !ok {
		if debug {
			fmt.Fprintf(os.Stderr, "[turnstile] 顶层不是数组: %T\n", parsed)
		}
		return "", false
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[turnstile] ops=%d\n", len(tokenList))
	}

	vm := &tsVM{vals: make(map[float64]any, 32), start: time.Now()}
	vm.vals[1] = tsCallable(vm.fn1)
	vm.vals[2] = tsCallable(vm.fn2)
	vm.vals[3] = tsCallable(vm.fn3)
	vm.vals[5] = tsCallable(vm.fn5)
	vm.vals[6] = tsCallable(vm.fn6)
	vm.vals[7] = tsCallable(vm.fn7)
	vm.vals[8] = tsCallable(vm.fn8)
	vm.vals[9] = tokenList
	vm.vals[10] = "window"
	vm.vals[14] = tsCallable(vm.fn14)
	vm.vals[15] = tsCallable(vm.fn15)
	vm.vals[16] = p
	vm.vals[17] = tsCallable(vm.fn17)
	vm.vals[18] = tsCallable(vm.fn18)
	vm.vals[19] = tsCallable(vm.fn19)
	vm.vals[20] = tsCallable(vm.fn20)
	vm.vals[21] = tsCallable(vm.fn21)
	vm.vals[23] = tsCallable(vm.fn23)
	vm.vals[24] = tsCallable(vm.fn24)

	for _, item := range tokenList {
		_ = item
	}

	// 指令队列可嵌套：执行过程中 9 号槽被替换为新队列（解密出的主脚本）时继续执行
	seen := map[uintptr]bool{}
	queue := tokenList
	for depth := 0; depth < 8; depth++ {
		vm.vals[9] = queue
		ptr := reflect.ValueOf(queue).Pointer()
		if seen[ptr] {
			break
		}
		seen[ptr] = true
		if debug {
			fmt.Fprintf(os.Stderr, "[turnstile] 队列[%d] ops=%d\n", depth, len(queue))
		}
		for i, item := range queue {
			tok, ok := item.([]any)
			if !ok || len(tok) == 0 {
				if debug && depth == 0 && i < 10 {
					fmt.Fprintf(os.Stderr, "[turnstile] op[%d] 非法: %T\n", i, item)
				}
				continue
			}
			idx, ok := toFloat(tok[0])
			if !ok {
				if debug && depth == 0 && i < 10 {
					fmt.Fprintf(os.Stderr, "[turnstile] op[%d] 索引非法: %v\n", i, tok[0])
				}
				continue
			}
			if fn, ok := vm.vals[idx].(tsCallable); ok {
				prevOut := vm.out
				fn(tok[1:]...)
				if debug {
					if vm.out != prevOut {
						fmt.Fprintf(os.Stderr, "[turnstile] 队列[%d] op[%d] slot=%v => out=%s\n", depth, i, tok[0], truncateStr(vm.out, 80))
					}
					if depth == 0 && i < 100 {
						extra := ""
						if vm.out != prevOut {
							extra = " => out 已设置"
						}
						fmt.Fprintf(os.Stderr, "[turnstile] op[%d] slot=%v args=%v%s\n", i, tok[0], truncateArgs(tok[1:]), extra)
					} else if depth > 0 && i >= len(queue)-25 {
						fmt.Fprintf(os.Stderr, "[turnstile] 队列[%d] op[%d] slot=%v args=%v\n", depth, i, tok[0], truncateArgs(tok[1:]))
					}
				}
			} else if debug && depth == 0 && i < 100 {
				fmt.Fprintf(os.Stderr, "[turnstile] op[%d] slot=%v 无过程（槽位值: %T）\n", i, tok[0], vm.vals[idx])
			}
		}
		next, _ := vm.vals[9].([]any)
		if next == nil || reflect.ValueOf(next).Pointer() == ptr {
			break
		}
		queue = next
	}
	if vm.out == "" {
		if debug {
			fmt.Fprintf(os.Stderr, "[turnstile] 输出为空\n")
		}
		return "", false
	}
	if debug {
		fmt.Fprintf(os.Stderr, "[turnstile] 输出=%s\n", truncateStr(vm.out, 120))
	}
	return vm.out, true
}

// truncateArgs 截断过长的调试参数（巨型混淆字符串）。
func truncateArgs(args []any) []any {
	out := make([]any, 0, len(args))
	for _, a := range args {
		if s, ok := a.(string); ok && len(s) > 40 {
			out = append(out, s[:40]+"...")
			continue
		}
		out = append(out, a)
	}
	return out
}

// fn1：xor 合并（vals[e] = xor(vals[e], vals[t])）。
func (vm *tsVM) fn1(args ...any) any {
	e, ok1 := argKey(args, 0)
	t, ok2 := argKey(args, 1)
	if ok1 && ok2 {
		vm.vals[e] = xorRunes(turnstileToStr(vm.vals[e]), turnstileToStr(vm.vals[t]))
	}
	return nil
}

// fn2：直接赋值（vals[e] = t）。
func (vm *tsVM) fn2(args ...any) any {
	e, ok := argKey(args, 0)
	if ok && len(args) >= 2 {
		vm.vals[e] = args[1]
	}
	return nil
}

// fn3：输出结果（参数为待编码值，base64 编码后作为最终 token）。
func (vm *tsVM) fn3(args ...any) any {
	if len(args) == 0 {
		return nil
	}
	vm.out = base64.StdEncoding.EncodeToString([]byte(turnstileToStr(args[0])))
	return nil
}

// fn5：拼接（列表追加或字符串连接）。
func (vm *tsVM) fn5(args ...any) any {
	e, ok1 := argKey(args, 0)
	t, ok2 := argKey(args, 1)
	if !ok1 || !ok2 {
		return nil
	}
	cur := vm.vals[e]
	inc := vm.vals[t]
	if list, ok := cur.([]any); ok {
		vm.vals[e] = append(append([]any{}, list...), inc)
		return nil
	}
	if isStrOrNum(cur) || isStrOrNum(inc) {
		vm.vals[e] = turnstileToStr(cur) + turnstileToStr(inc)
		return nil
	}
	vm.vals[e] = "NaN"
	return nil
}

// fn6：点号连接并特判 location。
func (vm *tsVM) fn6(args ...any) any {
	e, ok1 := argKey(args, 0)
	t, ok2 := argKey(args, 1)
	n, ok3 := argKey(args, 2)
	if !ok1 || !ok2 || !ok3 {
		return nil
	}
	tv, okT := vm.vals[t].(string)
	nv, okN := vm.vals[n].(string)
	if okT && okN {
		v := tv + "." + nv
		if v == "window.document.location" {
			v = baseURLChatGPT + "/"
		}
		vm.vals[e] = v
	}
	return nil
}

// fn7：调用目标（Reflect.set 或已注册过程）。
func (vm *tsVM) fn7(args ...any) any {
	e, ok := argKey(args, 0)
	if !ok {
		return nil
	}
	target := vm.vals[e]
	values := make([]any, 0, len(args))
	for _, a := range args[1:] {
		if idx, ok := toFloat(a); ok {
			values = append(values, vm.vals[idx])
		} else {
			values = append(values, a)
		}
	}
	if s, ok := target.(string); ok && s == "window.Reflect.set" {
		if len(values) >= 3 {
			if om, ok := values[0].(*tsOrderedMap); ok {
				om.add(turnstileToStr(values[1]), values[2])
			}
		}
		return nil
	}
	if fn, ok := target.(tsCallable); ok {
		fn(values...)
	}
	return nil
}

// fn8：复制（vals[e] = vals[t]）。
func (vm *tsVM) fn8(args ...any) any {
	e, ok1 := argKey(args, 0)
	t, ok2 := argKey(args, 1)
	if ok1 && ok2 {
		vm.vals[e] = vm.vals[t]
	}
	return nil
}

// fn14：JSON 解析。
func (vm *tsVM) fn14(args ...any) any {
	e, ok1 := argKey(args, 0)
	t, ok2 := argKey(args, 1)
	if !ok1 || !ok2 {
		return nil
	}
	if s, ok := vm.vals[t].(string); ok {
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) == nil {
			vm.vals[e] = v
		}
	}
	return nil
}

// fn15：JSON 序列化。
func (vm *tsVM) fn15(args ...any) any {
	e, ok1 := argKey(args, 0)
	t, ok2 := argKey(args, 1)
	if ok1 && ok2 {
		vm.vals[e] = string(marshalJSON(vm.vals[t]))
	}
	return nil
}

// fn17：浏览器 API 调用（performance.now / Object / Math.random 等）。
func (vm *tsVM) fn17(args ...any) any {
	e, ok1 := argKey(args, 0)
	t, ok2 := argKey(args, 1)
	if !ok1 || !ok2 {
		return nil
	}
	callArgs := make([]any, 0, len(args))
	for _, a := range args[2:] {
		if idx, ok := toFloat(a); ok {
			callArgs = append(callArgs, vm.vals[idx])
		} else {
			callArgs = append(callArgs, a)
		}
	}
	switch target := vm.vals[t].(type) {
	case string:
		switch target {
		case "window.performance.now":
			elapsed := float64(time.Since(vm.start).Nanoseconds()) / 1e6
			vm.vals[e] = json.Number(strconv.FormatFloat(elapsed+rand.Float64(), 'g', -1, 64))
		case "window.Object.create":
			vm.vals[e] = &tsOrderedMap{vals: map[string]any{}}
		case "window.Object.keys":
			if len(callArgs) > 0 {
				if s, ok := callArgs[0].(string); ok && s == "window.localStorage" {
					vm.vals[e] = []any{
						"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4",
						"STATSIG_LOCAL_STORAGE_STABLE_ID",
						"client-correlated-secret",
						"oai/apps/capExpiresAt",
						"oai-did",
						"STATSIG_LOCAL_STORAGE_LOGGING_REQUEST",
						"UiState.isNavigationCollapsed.1",
					}
				}
			}
		case "window.Math.random":
			vm.vals[e] = rand.Float64()
		}
	case tsCallable:
		vm.vals[e] = target(callArgs...)
	}
	return nil
}

// fn18：base64 解码。
func (vm *tsVM) fn18(args ...any) any {
	e, ok := argKey(args, 0)
	if ok {
		if s, ok := vm.vals[e].(string); ok {
			if b, err := base64.StdEncoding.DecodeString(s); err == nil {
				vm.vals[e] = string(b)
			}
		}
	}
	return nil
}

// fn19：base64 编码。
func (vm *tsVM) fn19(args ...any) any {
	e, ok := argKey(args, 0)
	if ok {
		vm.vals[e] = base64.StdEncoding.EncodeToString([]byte(turnstileToStr(vm.vals[e])))
	}
	return nil
}

// fn20：条件调用（vals[e] == vals[t] 时执行 vals[n]）。
func (vm *tsVM) fn20(args ...any) any {
	e, ok1 := argKey(args, 0)
	t, ok2 := argKey(args, 1)
	n, ok3 := argKey(args, 2)
	if !ok1 || !ok2 || !ok3 {
		return nil
	}
	if reflect.DeepEqual(vm.vals[e], vm.vals[t]) {
		if fn, ok := vm.vals[n].(tsCallable); ok {
			callArgs := make([]any, 0, len(args))
			for _, a := range args[3:] {
				if idx, ok := toFloat(a); ok {
					callArgs = append(callArgs, vm.vals[idx])
				}
			}
			fn(callArgs...)
		}
	}
	return nil
}

// fn21：空操作。
func (vm *tsVM) fn21(args ...any) any { return nil }

// fn23：条件间接调用（vals[e] 非空时执行 vals[t]）。
func (vm *tsVM) fn23(args ...any) any {
	e, ok1 := argKey(args, 0)
	t, ok2 := argKey(args, 1)
	if !ok1 || !ok2 {
		return nil
	}
	if vm.vals[e] != nil {
		if fn, ok := vm.vals[t].(tsCallable); ok {
			fn(args[2:]...)
		}
	}
	return nil
}

// fn24：点号连接。
func (vm *tsVM) fn24(args ...any) any {
	e, ok1 := argKey(args, 0)
	t, ok2 := argKey(args, 1)
	n, ok3 := argKey(args, 2)
	if ok1 && ok2 && ok3 {
		tv, okT := vm.vals[t].(string)
		nv, okN := vm.vals[n].(string)
		if okT && okN {
			vm.vals[e] = tv + "." + nv
		}
	}
	return nil
}
