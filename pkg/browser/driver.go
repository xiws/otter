// Package browser 提供基于 Playwright 的浏览器自动化驱动。
//
// BrowserDriver 封装了 Playwright 的启动、页面导航、元素操作和 Cookie 持久化，
// 供 ChatGPT 和 Gemini 等需要浏览器自动化的 Provider 使用。
package browser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mxschmitt/playwright-go"
)

// Driver 是浏览器驱动实例，包装 Playwright 的核心操作。
type Driver struct {
	pw          *playwright.Playwright
	browser     playwright.Browser
	context     playwright.BrowserContext
	page        playwright.Page
	cookiesPath string
	headless    bool

	started bool
}

// NewDriver 创建一个新的浏览器驱动。
//
//	cookiesPath: Cookie 持久化文件路径（空字符串表示不持久化）
//	headless:    是否无头模式（true 表示不显示浏览器窗口）
func NewDriver(cookiesPath string, headless bool) *Driver {
	return &Driver{
		cookiesPath: cookiesPath,
		headless:    headless,
	}
}

// Install 安装 Playwright 驱动和浏览器。
// 首次使用前需要调用一次（或在外部使用命令行安装）。
func Install() error {
	return playwright.Install(&playwright.RunOptions{
		Browsers: []string{"chromium"},
		Verbose:  true,
	})
}

// Start 启动浏览器并创建新页面。
func (d *Driver) Start() error {
	if d.started {
		return nil
	}

	pw, err := playwright.Run()
	if err != nil {
		return fmt.Errorf("启动 Playwright 失败（是否已执行 playwright.Install？）: %w", err)
	}
	d.pw = pw

	var browser playwright.Browser
	browser, err = pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(d.headless),
	})
	if err != nil {
		d.pw.Stop()
		return fmt.Errorf("启动浏览器失败: %w", err)
	}
	d.browser = browser

	context, err := browser.NewContext()
	if err != nil {
		d.browser.Close()
		d.pw.Stop()
		return fmt.Errorf("创建浏览器上下文失败: %w", err)
	}
	d.context = context

	// 尝试加载已保存的 cookie
	if d.cookiesPath != "" {
		if err := d.loadCookies(); err != nil {
			// cookie 文件不存在不算错误
		}
	}

	page, err := context.NewPage()
	if err != nil {
		d.context.Close()
		d.browser.Close()
		d.pw.Stop()
		return fmt.Errorf("创建页面失败: %w", err)
	}
	d.page = page

	d.started = true
	return nil
}

// Close 关闭浏览器和 Playwright 实例。
func (d *Driver) Close() {
	if !d.started {
		return
	}
	if d.cookiesPath != "" {
		_ = d.saveCookies()
	}
	if d.page != nil {
		_ = d.page.Close()
	}
	if d.context != nil {
		_ = d.context.Close()
	}
	if d.browser != nil {
		_ = d.browser.Close()
	}
	if d.pw != nil {
		_ = d.pw.Stop()
	}
	d.started = false
}

// Page 返回当前页面实例。
func (d *Driver) Page() playwright.Page {
	return d.page
}

// Context 返回当前浏览器上下文。
func (d *Driver) Context() playwright.BrowserContext {
	return d.context
}

// Navigate 导航到指定 URL 并等待页面加载完成。
func (d *Driver) Navigate(url string) error {
	if !d.started {
		return fmt.Errorf("浏览器未启动")
	}
	_, err := d.page.Goto(url, playwright.PageGotoOptions{
		WaitUntil: playwright.WaitUntilStateNetworkidle,
	})
	return err
}

// Evaluate 在页面中执行 JavaScript 表达式。
func (d *Driver) Evaluate(expression string, args ...any) (any, error) {
	if !d.started {
		return nil, fmt.Errorf("浏览器未启动")
	}
	return d.page.Evaluate(expression, args...)
}

// WaitForSelector 等待选择器匹配的元素出现。
func (d *Driver) WaitForSelector(selector string, timeout time.Duration) error {
	if !d.started {
		return fmt.Errorf("浏览器未启动")
	}
	timeoutMs := float64(timeout.Milliseconds())
	if timeoutMs <= 0 {
		timeoutMs = 30000 // 默认 30 秒
	}
	_, err := d.page.WaitForSelector(selector, playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(timeoutMs),
		State:   playwright.WaitForSelectorStateVisible,
	})
	return err
}

// Click 点击指定选择器的元素。
func (d *Driver) Click(selector string) error {
	if !d.started {
		return fmt.Errorf("浏览器未启动")
	}
	return d.page.Locator(selector).Click()
}

// Fill 在输入框中填入文本。
func (d *Driver) Fill(selector, text string) error {
	if !d.started {
		return fmt.Errorf("浏览器未启动")
	}
	return d.page.Locator(selector).Fill(text)
}

// Type 逐键输入文本（模拟键盘输入）。
func (d *Driver) Type(selector, text string) error {
	if !d.started {
		return fmt.Errorf("浏览器未启动")
	}
	return d.page.Locator(selector).PressSequentially(text)
}

// GetText 获取指定选择器元素的文本内容。
func (d *Driver) GetText(selector string) (string, error) {
	if !d.started {
		return "", fmt.Errorf("浏览器未启动")
	}
	return d.page.Locator(selector).TextContent()
}

// GetInputValue 获取输入框的当前值。
func (d *Driver) GetInputValue(selector string) (string, error) {
	if !d.started {
		return "", fmt.Errorf("浏览器未启动")
	}
	return d.page.Locator(selector).InputValue()
}

// IsVisible 检查选择器匹配的元素是否可见。
func (d *Driver) IsVisible(selector string) (bool, error) {
	if !d.started {
		return false, fmt.Errorf("浏览器未启动")
	}
	return d.page.Locator(selector).IsVisible()
}

// GetPageContent 返回当前页面的完整 HTML。
func (d *Driver) GetPageContent() (string, error) {
	if !d.started {
		return "", fmt.Errorf("浏览器未启动")
	}
	return d.page.Content()
}

// Screenshot 对当前页面截图（用于调试）。
func (d *Driver) Screenshot(path string) error {
	if !d.started {
		return fmt.Errorf("浏览器未启动")
	}
	_, err := d.page.Screenshot(playwright.PageScreenshotOptions{
		Path: playwright.String(path),
	})
	return err
}

// --- Cookie 管理 ---

// cookieFile 返回 cookie 文件路径。
func (d *Driver) cookieFile() string {
	if d.cookiesPath == "" {
		return ""
	}
	return d.cookiesPath
}

// saveCookies 将当前浏览器上下文的 cookie 保存到文件。
func (d *Driver) saveCookies() error {
	path := d.cookieFile()
	if path == "" {
		return nil
	}

	cookies, err := d.context.Cookies()
	if err != nil {
		return fmt.Errorf("获取 cookies 失败: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("创建 cookie 目录失败: %w", err)
	}

	data, err := json.MarshalIndent(cookies, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 cookies 失败: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("写入 cookie 文件失败: %w", err)
	}

	return nil
}

// loadCookies 从文件加载 cookie 到浏览器上下文。
func (d *Driver) loadCookies() error {
	path := d.cookieFile()
	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取 cookie 文件失败: %w", err)
	}

	var cookies []playwright.OptionalCookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		return fmt.Errorf("解析 cookies 失败: %w", err)
	}

	if len(cookies) > 0 {
		if err := d.context.AddCookies(cookies); err != nil {
			return fmt.Errorf("设置 cookies 失败: %w", err)
		}
	}

	return nil
}

// ClearCookies 清除所有 cookie（用于重新登录）。
func (d *Driver) ClearCookies() error {
	if !d.started {
		return nil
	}
	if err := d.context.ClearCookies(); err != nil {
		return fmt.Errorf("清除 cookies 失败: %w", err)
	}
	if d.cookiesPath != "" {
		os.Remove(d.cookiesPath)
	}
	return nil
}

// --- 辅助方法 ---

// WaitForLogin 等待用户在页面上完成登录。
// 通过定时检查页面 URL 是否不再停留在登录页面来判断登录状态。
// timeout: 最长等待时间
func (d *Driver) WaitForLogin(loginURLPrefix string, timeout time.Duration) error {
	if !d.started {
		return fmt.Errorf("浏览器未启动")
	}

	fmt.Fprintf(os.Stderr, "🔐 请在浏览器中完成登录（等待 %s）...\n", timeout)
	fmt.Fprintf(os.Stderr, "   如果浏览器已自动打开，请登录你的账号。\n")

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		url := d.page.URL()
		// 如果 URL 不再是登录页面，说明登录成功
		if url != "" && !contains(url, loginURLPrefix) && !contains(url, "accounts.google.com") {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("登录超时（%s），请重试或使用 otter auth login <provider> 手动登录", timeout)
}

// contains 判断字符串 s 是否包含 substr。
func contains(s, substr string) bool {
	return len(substr) > 0 && len(s) >= len(substr) &&
		searchString(s, substr)
}

// searchString 简单子串搜索。
func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// EnsurePlaywrightInstalled 检查 Playwright 是否可用，不可用时尝试安装。
func EnsurePlaywrightInstalled() error {
	// 尝试启动 Playwright 来检查是否已安装
	pw, err := playwright.Run(&playwright.RunOptions{
		Browsers:            []string{"chromium"},
		SkipInstallBrowsers: true,
	})
	if err == nil {
		pw.Stop()
		return nil
	}

	fmt.Fprintf(os.Stderr, "📥 Playwright 未安装，正在安装（仅首次需要）...\n")
	if err := playwright.Install(&playwright.RunOptions{
		Browsers: []string{"chromium"},
		Verbose:  true,
	}); err != nil {
		return fmt.Errorf("安装 Playwright 失败: %w\n请手动执行: go run github.com/mxschmitt/playwright-go/cmd/playwright install --driver", err)
	}

	fmt.Fprintf(os.Stderr, "✅ Playwright 安装完成\n")
	return nil
}
