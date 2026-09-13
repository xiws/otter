package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"otter/pkg/config"
	"otter/pkg/provider"
)

// newAuthCmd 创建认证管理命令组。
func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "管理登录认证",
		Long:  `登录各 AI 平台并检查登录状态。`,
	}

	cmd.AddCommand(newAuthCheckCmd())
	cmd.AddCommand(newAuthLoginCmd())

	return cmd
}

// newAuthCheckCmd 创建 otter auth check 命令。
func newAuthCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check [provider]",
		Short: "检查登录状态",
		Long: `检查各 AI 平台的认证状态。

不指定 provider 时，检查所有已配置的平台。

示例:
  otter auth check
  otter auth check deepseek`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			providers := []string{"chatgpt", "gemini", "deepseek"}
			if len(args) > 0 {
				providers = []string{resolveProvider(args[0])}
			}

			for _, p := range providers {
				status := checkProviderAuth(cmd.Context(), p)
				fmt.Printf("[%s] %s — %s\n", status.icon, p, status.msg)
			}

			return nil
		},
	}

	return cmd
}

// newAuthLoginCmd 创建 otter auth login 命令。
func newAuthLoginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login <provider>",
		Short: "登录到 AI 平台",
		Long: `使用配置的账号密码或浏览器登录 AI 平台并保存认证信息。

登录成功后，认证信息会被保存到配置文件中，后续消息发送命令会自动复用。

支持的 provider: chatgpt, gemini, deepseek

示例:
  otter auth login deepseek
  otter auth login deepseek --account user@example.com --password mypass
  otter auth login gpt
  otter auth login gem`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			providerName := resolveProvider(args[0])

			switch providerName {
			case "chatgpt":
				return chatGPTLogin(cmd)
			case "gemini":
				return geminiLogin(cmd)
			case "deepseek":
				return deepseekLogin(cmd)
			default:
				return fmt.Errorf("不支持的 provider: %s", providerName)
			}
		},
	}

	cmd.Flags().String("account", "", "登录账号（覆盖配置中的 account）")
	cmd.Flags().String("password", "", "登录密码（覆盖配置中的 password）")

	return cmd
}

// deepseekLogin 执行 DeepSeek 登录流程。
func deepseekLogin(cmd *cobra.Command) error {
	// 获取账号密码（优先级：命令行 > 环境变量 > 配置文件）
	account, _ := cmd.Flags().GetString("account")
	password, _ := cmd.Flags().GetString("password")

	if account == "" {
		account = appConfig.DeepSeek.Account
	}
	if password == "" {
		password = config.GetPassword(&appConfig.DeepSeek, "OTTER_DEEPSEEK_PASSWORD")
	}

	if account == "" {
		return fmt.Errorf("DeepSeek 账号未设置，请通过 --account 参数或配置 deepseek.account 设置")
	}
	if password == "" {
		return fmt.Errorf("DeepSeek 密码未设置，请通过 --password 参数、配置 deepseek.password 或环境变量 OTTER_DEEPSEEK_PASSWORD 设置")
	}

	fmt.Printf("🔑 正在登录 DeepSeek (账号: %s)...\n", maskString(account, 3))

	prov := provider.NewDeepSeekProvider()
	prov.SetAccount(account)
	prov.SetPassword(password)

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	if err := prov.Login(ctx); err != nil {
		return fmt.Errorf("登录失败: %w", err)
	}

	token := prov.Token()
	if token == "" {
		return fmt.Errorf("登录成功但未获取到 token")
	}

	// 保存 token 到配置
	appConfig.DeepSeek.AuthToken = token
	if err := config.Save(appConfig, cfgFile); err != nil {
		return fmt.Errorf("保存 token 到配置失败: %w", err)
	}

	fmt.Printf("✅ DeepSeek 登录成功！auth_token 已保存到配置文件\n")
	return nil
}

// chatGPTLogin 从本机 Chrome 自动提取 ChatGPT 登录凭据并校验。
//
// 自动提取失败时回退打印手动配置指引（session_token 通路）。
func chatGPTLogin(cmd *cobra.Command) error {
	cg := provider.NewChatGPTProvider()
	cg.SetCookiesPath(config.ResolvePath(appConfig.ChatGPT.CookiesPath))

	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()

	fmt.Println("🔍 正在从本机 Chrome 提取 ChatGPT 登录凭据...")
	if err := cg.Login(ctx); err != nil {
		printChatGPTManualGuide()
		return fmt.Errorf("自动提取登录失败: %v", err)
	}

	if account := cg.Account(); account != "" {
		fmt.Printf("✅ ChatGPT 登录成功！账号: %s\n", account)
	} else {
		fmt.Println("✅ ChatGPT 登录成功！cookie 已保存")
	}
	return nil
}

// printChatGPTManualGuide 打印手动配置 session_token 的指引。
func printChatGPTManualGuide() {
	fmt.Println("")
	fmt.Println("  自动提取失败，可手动配置 session_token：")
	fmt.Println("")
	fmt.Println("  步骤：")
	fmt.Println("    1. 用 Chrome 打开 https://chatgpt.com 并登录")
	fmt.Println("    2. 按 F12 → 切换到 Application 标签")
	fmt.Println("    3. 左侧展开 Cookies → 选择 https://chatgpt.com")
	fmt.Println("    4. 找到 __Secure-next-auth.session-token 行")
	fmt.Println("    5. 双击 Value 列复制完整的 token 值")
	fmt.Println("    6. 执行以下命令保存：")
	fmt.Println("")
	fmt.Println("       otter config set chatgpt.session_token \"<复制的值>\"")
	fmt.Println("")
	fmt.Println("  或者通过环境变量传入：")
	fmt.Println("       export OTTER_CHATGPT_SESSION_TOKEN=\"<复制的值>\"")
	fmt.Println("")
	fmt.Println("  session_token 有效期很长，一次提取可长期使用。")
	fmt.Println("  过期后重新执行一次以上步骤即可。")
}

// geminiLogin 从本机 Chrome 自动提取 Google 凭据并校验 Gemini 登录态。
func geminiLogin(cmd *cobra.Command) error {
	gm := provider.NewGeminiProvider()
	gm.SetCookiesPath(config.ResolvePath(appConfig.Gemini.CookiesPath))
	if appConfig.Gemini.Language != "" {
		gm.SetLanguage(appConfig.Gemini.Language)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
	defer cancel()

	fmt.Println("🔍 正在从本机 Chrome 提取 Google 登录凭据...")
	if err := gm.Login(ctx); err != nil {
		return fmt.Errorf("Gemini 登录失败: %w\n请确认 Chrome 已登录 gemini.google.com 后重试", err)
	}

	fmt.Println("✅ Gemini 登录成功！cookie 已保存")
	return nil
}

type authStatus struct {
	icon string
	msg  string
}

// checkProviderAuth 检查指定 provider 的认证状态。
//
// ChatGPT/Gemini 执行真实网络校验（换取登录态 / 打开页面提取令牌），
// 不再仅检查配置字段是否存在。
func checkProviderAuth(ctx context.Context, providerName string) authStatus {
	switch providerName {
	case "deepseek":
		// 优先环境变量
		if v := os.Getenv("OTTER_DEEPSEEK_AUTH_TOKEN"); v != "" {
			return authStatus{icon: "✅", msg: fmt.Sprintf("已认证（环境变量，token 前 %d 位: %s）", minInt(8, len(v)), v[:minInt(8, len(v))])}
		}
		// 配置文件中的 token
		if appConfig != nil && appConfig.DeepSeek.AuthToken != "" {
			t := appConfig.DeepSeek.AuthToken
			return authStatus{icon: "✅", msg: fmt.Sprintf("已认证（配置文件，token 前 %d 位: %s）", minInt(8, len(t)), t[:minInt(8, len(t))])}
		}
		// 有账号密码但未登录
		if appConfig != nil && appConfig.DeepSeek.Account != "" && appConfig.DeepSeek.Password != "" {
			return authStatus{icon: "⚠️", msg: "账号密码已配置，请执行 otter auth login deepseek 完成登录"}
		}
		if appConfig != nil && appConfig.DeepSeek.Account != "" {
			return authStatus{icon: "⚠️", msg: "账号已配置但密码未设置"}
		}
		return authStatus{icon: "❌", msg: "未配置认证信息"}
	case "chatgpt":
		cg := provider.NewChatGPTProvider()
		_ = initChatGPTProvider(cg)
		vctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if _, err := cg.ValidateSession(vctx); err != nil {
			return authStatus{icon: "❌", msg: fmt.Sprintf("登录态无效: %s", firstLine(err.Error()))}
		}
		if account := cg.Account(); account != "" {
			return authStatus{icon: "✅", msg: fmt.Sprintf("已登录（真实校验通过，账号: %s）", account)}
		}
		return authStatus{icon: "✅", msg: "已登录（真实校验通过）"}
	case "gemini":
		gm := provider.NewGeminiProvider()
		_ = initGeminiProvider(gm)
		vctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if _, err := gm.ValidateSession(vctx); err != nil {
			return authStatus{icon: "❌", msg: fmt.Sprintf("登录态无效: %s", firstLine(err.Error()))}
		}
		return authStatus{icon: "✅", msg: "已登录（真实校验通过）"}
	default:
		return authStatus{icon: "❓", msg: "未知 provider"}
	}
}

// firstLine 返回字符串的首行（用于压缩多行错误信息）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// minInt 返回两个整数中的较小值。
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
