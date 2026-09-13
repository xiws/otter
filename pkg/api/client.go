// Package api 提供 HTTP 客户端封装，支持 cookie 管理、重试与超时。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client 封装 http.Client，提供重试、超时和 cookie 管理能力。
type Client struct {
	httpClient *http.Client
	baseURL    string
	headers    map[string]string
	authToken  string
	userAgent  string
}

// NewClient 创建一个新的 API 客户端。
func NewClient(baseURL string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		baseURL:   baseURL,
		headers:   make(map[string]string),
		userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	}
}

// SetHeader 设置对所有请求生效的默认请求头（如 Cookie）。
//
// 请求级头部（Content-Type、x-ds-pow-response 等）不要用这里设置：
// 它们会留在 map 里并污染后续请求，请改用 DoOnce/PostRaw 的 extra 参数。
func (c *Client) SetHeader(key, value string) {
	c.headers[key] = value
}

// SetAuthToken 设置认证 token。
func (c *Client) SetAuthToken(token string) {
	c.authToken = token
}

// SetTimeout 设置 HTTP 客户端超时。设为 0 表示不限制（改由 context 控制），
// 流式响应必须这样设置，否则长回复会被固定超时截断。
func (c *Client) SetTimeout(d time.Duration) {
	c.httpClient.Timeout = d
}

// newRequest 构造请求，extra 中的同名字段会覆盖默认请求头。
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader, extra map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	req.Header.Set("User-Agent", c.userAgent)
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	for k, v := range extra {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	return req, nil
}

// Do 执行 HTTP 请求，失败时重试。
//
// 仅用于幂等操作。请求体会被完整读入内存以便重试时重放，
// 因此不适合超大请求体。
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.doWithRetry(ctx, method, path, body, nil, 2)
}

// DoOnce 执行请求但不重试，extra 为本次请求专属的请求头。
//
// 用于非幂等操作（发送消息、上传文件等）：重试会造成重复提交。
func (c *Client) DoOnce(ctx context.Context, method, path string, body io.Reader, extra map[string]string) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, path, body, extra)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

// doWithRetry 执行带有重试的 HTTP 请求。
func (c *Client) doWithRetry(ctx context.Context, method, path string, body io.Reader, extra map[string]string, maxRetries int) (*http.Response, error) {
	// 先把请求体读进内存，重试时才能完整重放；
	// 否则第二次请求会发送空 body。
	var bodyBytes []byte
	if body != nil {
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("读取请求体失败: %w", err)
		}
		bodyBytes = b
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// 指数退避：1s, 2s
			backoff := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		var reader io.Reader
		if bodyBytes != nil {
			reader = bytes.NewReader(bodyBytes)
		}
		req, err := c.newRequest(ctx, method, path, reader, extra)
		if err != nil {
			return nil, err
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("请求失败(尝试 %d/%d): %w", attempt+1, maxRetries+1, err)
			continue
		}
		return resp, nil
	}

	return nil, fmt.Errorf("重试 %d 次后仍失败: %w", maxRetries, lastErr)
}

// GetJSON 发送 GET 请求并解析 JSON 响应。
func (c *Client) GetJSON(ctx context.Context, path string, result interface{}) error {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

// PostJSON 发送 POST JSON 请求并解析 JSON 响应。
func (c *Client) PostJSON(ctx context.Context, path string, reqBody, result interface{}) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("编码请求体失败: %w", err)
	}

	resp, err := c.DoOnce(ctx, http.MethodPost, path, bytes.NewReader(body),
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

// PostRaw 发送原始请求体并返回响应字节，不重试。
func (c *Client) PostRaw(ctx context.Context, path string, contentType string, body io.Reader) ([]byte, error) {
	return c.PostRawWithHeaders(ctx, path, contentType, body, nil)
}

// PostRawWithHeaders 发送原始请求体并附带本次请求专属的请求头，不重试。
func (c *Client) PostRawWithHeaders(ctx context.Context, path string, contentType string, body io.Reader, extra map[string]string) ([]byte, error) {
	headers := make(map[string]string, len(extra)+1)
	for k, v := range extra {
		headers[k] = v
	}
	if contentType != "" {
		headers["Content-Type"] = contentType
	}

	resp, err := c.DoOnce(ctx, http.MethodPost, path, body, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return io.ReadAll(resp.Body)
}

// DoRaw 执行请求并返回原始响应体字节。
func (c *Client) DoRaw(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	resp, err := c.Do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// SetCookie 设置 Cookie 请求头。
func (c *Client) SetCookie(cookie string) {
	c.SetHeader("Cookie", cookie)
}
