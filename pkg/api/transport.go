package api

import (
	"crypto/rand"
	"fmt"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// chromeUA 是与 tls-client 的 Chrome_152 指纹匹配的 User-Agent。
const chromeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// newBrowserClient 构造伪装为 Chrome 的 HTTP 客户端。
//
// ChatGPT/Cloudflare 会根据 TLS/HTTP2 指纹拦截标准 net/http 客户端，
// 必须使用 tls-client 的浏览器指纹配置才能直连。
func newBrowserClient(timeout time.Duration) (tls_client.HttpClient, error) {
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(int(timeout.Seconds())),
		tls_client.WithClientProfile(profiles.Chrome_152),
	)
	if err != nil {
		return nil, fmt.Errorf("创建浏览器指纹 HTTP 客户端失败: %w", err)
	}
	return client, nil
}

// newUUIDv4 生成随机的 UUID v4 字符串。
func newUUIDv4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败属于极端情况，退回时间戳形式保证唯一性
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xFFFFFFFFFFFF)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
