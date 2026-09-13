package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"otter/pkg/config"
)

// newConfigCmd 创建配置管理命令组。
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "管理配置",
		Long:  `查看和修改 otter 配置。`,
	}

	cmd.AddCommand(newConfigShowCmd())
	cmd.AddCommand(newConfigSetCmd())

	return cmd
}

// newConfigShowCmd 创建 otter config show 命令。
func newConfigShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "查看当前配置",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := appConfig
			if cfg == nil {
				return fmt.Errorf("配置未加载，请检查 --config 参数")
			}

			fmt.Println("=== 全局配置 ===")
			fmt.Printf("  浏览器可见:    %v\n", cfg.Global.BrowserVisible)
			fmt.Printf("  默认超时:      %s\n", cfg.Global.DefaultTimeout)
			fmt.Printf("  默认输出格式:  %s\n", cfg.Global.DefaultOutput)
			fmt.Printf("  数据目录:      %s\n", cfg.Global.DataDir)

			fmt.Println("\n=== ChatGPT ===")
			printProviderConfig(cfg.ChatGPT, "chatgpt")

			fmt.Println("\n=== Gemini ===")
			printProviderConfig(cfg.Gemini, "gemini")

			fmt.Println("\n=== DeepSeek ===")
			printProviderConfig(cfg.DeepSeek, "deepseek")

			fmt.Printf("\n配置文件路径: %s\n", cfgFile)
			return nil
		},
	}

	return cmd
}

// printProviderConfig 输出 provider 配置。
func printProviderConfig(cfg config.ProviderConfig, name string) {
	fmt.Printf("  Account:      %s\n", maskString(cfg.Account, 3))
	fmt.Printf("  Auth Type:    %s\n", cfg.AuthType)
	fmt.Printf("  Cookies:      %s\n", cfg.CookiesPath)
	if cfg.Model != "" {
		fmt.Printf("  Model:        %s\n", cfg.Model)
	}
	if cfg.Language != "" {
		fmt.Printf("  Language:     %s\n", cfg.Language)
	}

	// session_token 只对 ChatGPT 有意义
	if name == "chatgpt" && cfg.SessionToken != "" {
		fmt.Printf("  Session Token: %s\n", maskString(cfg.SessionToken, 12))
	}

	envKey := fmt.Sprintf("OTTER_%s_AUTH_TOKEN", upperEnvKey(name))
	if v := os.Getenv(envKey); v != "" {
		fmt.Printf("  Auth Token:    %s (来自环境变量)\n", maskString(v, 8))
	} else if cfg.AuthToken != "" {
		fmt.Printf("  Auth Token:    %s (已保存)\n", maskString(cfg.AuthToken, 8))
	} else if cfg.Password != "" {
		fmt.Printf("  Password:      %s\n", maskString(cfg.Password, 3))
	}

	// 同时也显示 session_token 环境变量
	if name == "chatgpt" {
		if v := os.Getenv("OTTER_CHATGPT_SESSION_TOKEN"); v != "" {
			fmt.Printf("  Session Token: %s (来自环境变量)\n", maskString(v, 12))
		}
	}
}

// newConfigSetCmd 创建 otter config set 命令。
func newConfigSetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "设置配置项",
		Long: `设置配置项的值。

支持的 key 格式:
  chatgpt.account         - ChatGPT 账号
  chatgpt.password        - ChatGPT 密码
  chatgpt.session_token   - ChatGPT 的 __Secure-next-auth.session-token
  chatgpt.cookies_path    - ChatGPT cookie 持久化文件路径
  chatgpt.model           - ChatGPT 模型标识（默认 auto，如 gpt-5-5）
  gemini.account          - Gemini 账号
  gemini.password         - Gemini 密码
  gemini.cookies_path     - Gemini cookie 持久化文件路径
  gemini.language         - Gemini 请求语言（默认 en，如 zh-CN）
  deepseek.account        - DeepSeek 账号
  deepseek.password       - DeepSeek 密码
  deepseek.auth_token     - DeepSeek 直接设置 auth_token
  global.browser_visible  - 浏览器可见模式 (true/false)
  global.default_timeout  - 默认超时 (例如 5m)
  global.default_output   - 默认输出格式 (text/markdown/raw)

示例:
  otter config set deepseek.account 15170000000
  otter config set chatgpt.model gpt-5-5
  otter config set global.default_timeout 10m
  otter config set global.browser_visible true`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key := args[0]
			value := args[1]

			cfg := appConfig
			if cfg == nil {
				return fmt.Errorf("配置未加载")
			}

			if err := setConfigValue(cfg, key, value); err != nil {
				return err
			}

			// 保存配置
			if err := config.Save(cfg, cfgFile); err != nil {
				return fmt.Errorf("保存配置失败: %w", err)
			}

			fmt.Printf("✅ 已设置 %s = %s\n", key, value)
			return nil
		},
	}

	return cmd
}

// setConfigValue 根据 key 设置配置值。
func setConfigValue(cfg *config.Config, key, value string) error {
	switch key {
	case "chatgpt.account":
		cfg.ChatGPT.Account = value
	case "chatgpt.password":
		cfg.ChatGPT.Password = value
	case "chatgpt.session_token":
		cfg.ChatGPT.SessionToken = value
	case "chatgpt.cookies_path":
		cfg.ChatGPT.CookiesPath = value
	case "chatgpt.model":
		cfg.ChatGPT.Model = value
	case "gemini.account":
		cfg.Gemini.Account = value
	case "gemini.password":
		cfg.Gemini.Password = value
	case "gemini.cookies_path":
		cfg.Gemini.CookiesPath = value
	case "gemini.language":
		cfg.Gemini.Language = value
	case "deepseek.account":
		cfg.DeepSeek.Account = value
	case "deepseek.password":
		cfg.DeepSeek.Password = value
	case "deepseek.auth_token":
		cfg.DeepSeek.AuthToken = value
	case "global.browser_visible":
		cfg.Global.BrowserVisible = value == "true"
	case "global.default_timeout":
		cfg.Global.DefaultTimeout = value
	case "global.default_output":
		cfg.Global.DefaultOutput = value
	default:
		return fmt.Errorf("不支持的配置项: %s", key)
	}
	return nil
}

// maskString 对字符串进行脱敏显示。
func maskString(s string, visible int) string {
	if s == "" {
		return "(未设置)"
	}
	if len(s) <= visible {
		return s
	}
	masked := s[:visible]
	for i := visible; i < len(s); i++ {
		masked += "*"
	}
	return masked
}

// upperEnvKey 将 provider 名称转为环境变量 key 的大写形式。
func upperEnvKey(s string) string {
	switch s {
	case "chatgpt":
		return "CHATGPT"
	case "gemini":
		return "GEMINI"
	case "deepseek":
		return "DEEPSEEK"
	}
	return s
}
