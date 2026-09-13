// Package chrome 从本地 Google Chrome 浏览器中提取并解密 cookie。
//
// 目前仅支持 macOS：Chrome 使用 AES-128-CBC(v10) 加密 cookie，
// 密钥来自 Keychain 中的 "Chrome Safe Storage" 条目。
package chrome

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"
)

// Cookie 表示从 Chrome 提取的一个 cookie。
type Cookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Domain   string    `json:"domain"`
	Path     string    `json:"path,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"http_only,omitempty"`
}

const (
	chromeSafeStorageService = "Chrome Safe Storage"
	chromeSafeStorageAccount = "Chrome"
	pbkdf2Salt               = "saltysalt"
	pbkdf2Iterations         = 1003
	pbkdf2KeyLen             = 16
)

// 1601-01-01 (Chrome 纪元) 与 1970-01-01 的秒差。
const chromeEpochOffset = 11644473600

// ExtractCookies 从 Chrome 的各个 profile 中提取指定域名的 cookie。
//
// domains 传入不带协议的域名（如 "chatgpt.com"），会匹配该域名及其子域。
// profile 按 Default → Profile N 的顺序尝试，返回第一个有匹配结果
// 的 profile 的数据（避免混合不同 profile 的登录态）。
func ExtractCookies(domains ...string) ([]Cookie, error) {
	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("自动提取 Chrome cookie 目前仅支持 macOS（当前系统: %s）", runtime.GOOS)
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("未指定要提取的域名")
	}

	userDataDir, err := chromeUserDataDir()
	if err != nil {
		return nil, err
	}

	profiles := listProfiles(userDataDir)
	if len(profiles) == 0 {
		return nil, fmt.Errorf("未找到任何 Chrome profile 目录（%s）", userDataDir)
	}

	var lastErr error
	for _, profile := range profiles {
		cookies, err := extractFromProfile(filepath.Join(userDataDir, profile), domains)
		if err != nil {
			lastErr = err
			continue
		}
		if len(cookies) > 0 {
			return cookies, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("未在 Chrome 中找到 %s 的 cookie，请确认已在 Chrome 中登录对应网站",
		strings.Join(domains, "/"))
}

// CookieHeader 将 cookie 列表拼接为 HTTP Cookie 请求头的值。
func CookieHeader(cookies []Cookie) string {
	var b strings.Builder
	for i, c := range cookies {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteByte('=')
		b.WriteString(c.Value)
	}
	return b.String()
}

// SaveCookies 将 cookie 列表保存为 JSON 文件。
func SaveCookies(path string, cookies []Cookie) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("创建 cookie 目录失败: %w", err)
	}
	data, err := json.MarshalIndent(cookies, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 cookie 失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("写入 cookie 文件失败: %w", err)
	}
	return nil
}

// LoadCookies 从 JSON 文件加载 cookie 列表。
func LoadCookies(path string) ([]Cookie, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cookies []Cookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		return nil, fmt.Errorf("解析 cookie 文件失败: %w", err)
	}
	return cookies, nil
}

// chromeUserDataDir 返回 macOS 上 Chrome 的用户数据目录。
func chromeUserDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户目录失败: %w", err)
	}
	dir := filepath.Join(home, "Library", "Application Support", "Google", "Chrome")
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("未找到 Chrome 数据目录（%s）: %w", dir, err)
	}
	return dir, nil
}

// listProfiles 列出包含 Cookies 数据库的 profile 目录名（Default 排最前）。
func listProfiles(userDataDir string) []string {
	entries, err := os.ReadDir(userDataDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name != "Default" && !strings.HasPrefix(name, "Profile ") {
			continue
		}
		if _, err := os.Stat(filepath.Join(userDataDir, name, "Cookies")); err == nil {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "Default" {
			return true
		}
		if names[j] == "Default" {
			return false
		}
		return names[i] < names[j]
	})
	return names
}

// dbRow 是 cookies 表中的一行。
type dbRow struct {
	HostKey    string `json:"host_key"`
	Name       string `json:"name"`
	Value      string `json:"value"`
	EncHex     string `json:"enc"`
	Path       string `json:"path"`
	ExpiresUTC int64  `json:"expires_utc"`
	IsSecure   int    `json:"is_secure"`
	IsHTTPOnly int    `json:"is_httponly"`
}

// extractFromProfile 从单个 profile 提取 cookie。
func extractFromProfile(profileDir string, domains []string) ([]Cookie, error) {
	tmpDir, err := os.MkdirTemp("", "otter-chrome-cookies-")
	if err != nil {
		return nil, fmt.Errorf("创建临时目录失败: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath, err := copyCookieDB(profileDir, tmpDir)
	if err != nil {
		return nil, err
	}

	rows, err := queryCookies(dbPath, domains)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	pw, err := keychainPassword()
	if err != nil {
		return nil, err
	}
	key := pbkdf2.Key([]byte(pw), []byte(pbkdf2Salt), pbkdf2Iterations, pbkdf2KeyLen, sha1.New)

	now := time.Now()
	var cookies []Cookie
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		if seen[r.Name] {
			continue
		}
		seen[r.Name] = true

		expires := chromeTime(r.ExpiresUTC)
		if !expires.IsZero() && expires.Before(now) {
			continue
		}

		value, err := decryptRow(r, key)
		if err != nil {
			// 单个 cookie 解密失败不阻断整体提取
			continue
		}
		value = sanitizeCookieValue(value)
		if value == "" {
			continue
		}

		cookies = append(cookies, Cookie{
			Name:     r.Name,
			Value:    value,
			Domain:   r.HostKey,
			Path:     r.Path,
			Expires:  expires,
			Secure:   r.IsSecure != 0,
			HTTPOnly: r.IsHTTPOnly != 0,
		})
	}
	return cookies, nil
}

// copyCookieDB 将 Cookies 数据库（含 WAL/SHM）复制到临时目录。
//
// Chrome 运行时会对数据库加锁，直接读取可能失败，复制后读取更稳妥。
func copyCookieDB(profileDir, dstDir string) (string, error) {
	src := filepath.Join(profileDir, "Cookies")
	dst := filepath.Join(dstDir, "Cookies")
	if err := copyFile(src, dst); err != nil {
		return "", fmt.Errorf("复制 cookie 数据库失败: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(src + suffix); err == nil {
			_ = copyFile(src+suffix, dst+suffix)
		}
	}
	return dst, nil
}

// copyFile 复制文件内容与权限。
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0600)
}

// queryCookies 通过 sqlite3 CLI 查询 cookie 行（避免 CGO 依赖）。
func queryCookies(dbPath string, domains []string) ([]dbRow, error) {
	conds := make([]string, 0, len(domains)*3)
	for _, d := range domains {
		d = strings.TrimPrefix(strings.TrimSpace(d), ".")
		conds = append(conds,
			fmt.Sprintf("host_key = '%s'", d),
			fmt.Sprintf("host_key = '.%s'", d),
			fmt.Sprintf("host_key LIKE '%%.%s'", d))
	}
	query := "SELECT host_key, name, value, hex(encrypted_value) AS enc, path, expires_utc, is_secure, is_httponly " +
		"FROM cookies WHERE " + strings.Join(conds, " OR ")

	out, err := exec.Command("sqlite3", "-json", dbPath, query).Output()
	if err != nil {
		return nil, fmt.Errorf("查询 cookie 数据库失败: %w", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}

	var rows []dbRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("解析 cookie 查询结果失败: %w", err)
	}
	return rows, nil
}

// keychainPassword 从 Keychain 读取 Chrome 的加密密钥口令。
func keychainPassword() (string, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-w", "-a", chromeSafeStorageAccount, "-s", chromeSafeStorageService).Output()
	if err != nil {
		return "", fmt.Errorf("读取 Keychain 中的 Chrome Safe Storage 失败（首次使用时请在弹窗中允许访问）: %w", err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// decryptRow 解密一行 cookie 的取值。
func decryptRow(r dbRow, key []byte) (string, error) {
	if r.EncHex == "" {
		return r.Value, nil
	}
	enc, err := hex.DecodeString(r.EncHex)
	if err != nil {
		return "", fmt.Errorf("解码密文失败: %w", err)
	}
	return decryptValue(enc, r.HostKey, key)
}

// decryptValue 解密 Chrome v10 格式的 cookie 密文。
//
// 格式：v10 前缀 + AES-128-CBC 密文；解密后前 32 字节为新版 Chrome
// 写入的 SHA256(host_key) 校验前缀（校验通过则剥离），末尾为 PKCS7 padding。
func decryptValue(enc []byte, hostKey string, key []byte) (string, error) {
	if bytes.HasPrefix(enc, []byte("v20")) {
		return "", fmt.Errorf("cookie 使用了 v20 (App-Bound) 加密，暂不支持自动解密")
	}
	if !bytes.HasPrefix(enc, []byte("v10")) {
		// 非加密存储（旧格式明文）
		return string(enc), nil
	}

	ciphertext := enc[3:]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return "", fmt.Errorf("密文长度非法: %d", len(ciphertext))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("初始化 AES 失败: %w", err)
	}
	iv := bytes.Repeat([]byte{' '}, aes.BlockSize)
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ciphertext)

	if len(plain) > 32 {
		sum := sha256.Sum256([]byte(hostKey))
		if bytes.Equal(plain[:32], sum[:]) {
			plain = plain[32:]
		}
	}
	plain = pkcs7Unpad(plain)
	return string(plain), nil
}

// pkcs7Unpad 去除 PKCS7 padding；padding 非法时原样返回。
func pkcs7Unpad(b []byte) []byte {
	n := len(b)
	if n == 0 {
		return b
	}
	pad := int(b[n-1])
	if pad < 1 || pad > aes.BlockSize || pad > n {
		return b
	}
	for i := n - pad; i < n; i++ {
		if int(b[i]) != pad {
			return b
		}
	}
	return b[:n-pad]
}

// chromeTime 将 Chrome 的微秒时间戳（1601 纪元）转换为 time.Time。
func chromeTime(micros int64) time.Time {
	if micros == 0 {
		return time.Time{} // session cookie
	}
	secs := micros/1_000_000 - chromeEpochOffset
	if secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}

// sanitizeCookieValue 过滤控制字符，保证可安全写入 Cookie 请求头。
func sanitizeCookieValue(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
