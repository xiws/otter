// Package config 提供配置文件的读取、写入与校验功能。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ProviderConfig 存储单个 AI 平台的认证配置。
type ProviderConfig struct {
	Account      string `json:"account,omitempty"`
	Password     string `json:"password,omitempty"`
	SessionToken string `json:"session_token,omitempty"` // ChatGPT __Secure-next-auth.session-token
	AuthType     string `json:"auth_type,omitempty"`     // "google" | "deepseek"
	CookiesPath  string `json:"cookies_path,omitempty"`
	AuthToken    string `json:"auth_token,omitempty"` // 登录后保存的 token，保存到配置文件
	Model        string `json:"model,omitempty"`      // 平台模型标识（如 ChatGPT 的 auto/gpt-5-5）
	Language     string `json:"language,omitempty"`   // 请求语言（如 Gemini 的 en/zh-CN），空为默认
}

// GlobalConfig 存储全局设置。
type GlobalConfig struct {
	BrowserVisible bool   `json:"browser_visible"`
	DefaultTimeout string `json:"default_timeout"` // e.g. "5m"
	DefaultOutput  string `json:"default_output"`  // "text" | "markdown" | "raw"
	DataDir        string `json:"data_dir"`        // default: ~/.otter/
}

// Config 是整个配置文件的顶层结构。
type Config struct {
	ChatGPT  ProviderConfig `json:"chatgpt"`
	Gemini   ProviderConfig `json:"gemini"`
	DeepSeek ProviderConfig `json:"deepseek"`
	Global   GlobalConfig   `json:"global"`
}

// DefaultConfig 返回一份合理的默认配置。
func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, ".otter")

	return &Config{
		ChatGPT: ProviderConfig{
			AuthType:    "google",
			CookiesPath: filepath.Join(dataDir, "sessions", "chatgpt", "cookies.json"),
		},
		Gemini: ProviderConfig{
			AuthType:    "google",
			CookiesPath: filepath.Join(dataDir, "sessions", "gemini", "cookies.json"),
		},
		DeepSeek: ProviderConfig{
			AuthType:    "deepseek",
			CookiesPath: filepath.Join(dataDir, "sessions", "deepseek", "cookies.json"),
		},
		Global: GlobalConfig{
			BrowserVisible: false,
			DefaultTimeout: "5m",
			DefaultOutput:  "text",
			DataDir:        dataDir,
		},
	}
}

// Load 从指定路径加载配置文件。如果文件不存在则返回默认配置。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	return cfg, nil
}

// Save 将配置写入指定路径。
func Save(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("写入配置文件 %s 失败: %w", path, err)
	}
	return nil
}

// ResolvePath 将 data-dir 参数或配置中的路径解析为绝对路径。
func ResolvePath(p string) string {
	if len(p) > 0 && p[0] == '~' {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, p[1:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// GetPassword 从环境变量或配置中获取指定 provider 的密码。
// 环境变量优先级高于配置文件。
func GetPassword(cfg *ProviderConfig, envKey string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return cfg.Password
}

// GetAuthToken 从环境变量或配置中获取指定 provider 的 auth_token。
// 环境变量优先级高于配置文件。
func GetAuthToken(cfg *ProviderConfig, envKey string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return cfg.AuthToken
}

// EnsureDataDir 确保数据目录存在。
func EnsureDataDir(dataDir string) error {
	dirs := []string{
		dataDir,
		filepath.Join(dataDir, "sessions"),
		filepath.Join(dataDir, "sessions", "chatgpt"),
		filepath.Join(dataDir, "sessions", "gemini"),
		filepath.Join(dataDir, "sessions", "deepseek"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return fmt.Errorf("创建数据目录 %s 失败: %w", d, err)
		}
	}
	return nil
}
