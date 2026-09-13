package cli

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"otter/pkg/session"
)

// newSessionCmd 创建会话管理命令组。
func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "管理会话",
		Long:  `创建、选择、列举、查看和删除 AI 对话会话。`,
	}

	cmd.AddCommand(newSessionNewCmd())
	cmd.AddCommand(newSessionSelectCmd())
	cmd.AddCommand(newSessionListCmd())
	cmd.AddCommand(newSessionShowCmd())
	cmd.AddCommand(newSessionRMCmd())

	return cmd
}

// newSessionNewCmd 创建 otter new 命令。
func newSessionNewCmd() *cobra.Command {
	var title string

	cmd := &cobra.Command{
		Use:   "new <provider>",
		Short: "创建新会话",
		Long: `创建一个新的 AI 对话会话。

支持的 provider: chatgpt, gemini, deepseek（可用别名: gpt, gem, dep）

示例:
  otter session new deepseek
  otter session new gpt --title "代码审查"
  otter session new gem --title "翻译助手"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := resolveProvider(args[0])

			sessionMgr := session.NewSessionManager(dataDir)

			sess, err := sessionMgr.Create(provider, title)
			if err != nil {
				return fmt.Errorf("创建会话失败: %w", err)
			}

			// 设置为活动会话
			if err := sessionMgr.SetActive(sess.ID); err != nil {
				logDebug("设置活动会话失败: %v", err)
			}

			fmt.Printf("✅ 已创建新会话: %s\n", sess.ID)
			if title != "" {
				fmt.Printf("   标题: %s\n", title)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "会话标题")

	return cmd
}

// newSessionSelectCmd 创建 otter select 命令。
func newSessionSelectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "select <provider> <session-id>",
		Short: "选择活动会话",
		Long: `将指定会话设置为当前活动会话。

后续的发送命令将默认使用该会话。

示例:
  otter session select deepseek sess_abc123
  otter session select gpt sess_def456`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[1]

			sessionMgr := session.NewSessionManager(dataDir)

			sess, err := sessionMgr.Get(sessionID)
			if err != nil {
				return fmt.Errorf("获取会话失败: %w", err)
			}
			if sess == nil {
				return fmt.Errorf("会话 %s 不存在", sessionID)
			}

			if err := sessionMgr.SetActive(sessionID); err != nil {
				return fmt.Errorf("设置活动会话失败: %w", err)
			}

			fmt.Printf("✅ 已切换到会话 %s (%s)\n", sess.Title, sess.ID)
			return nil
		},
	}

	return cmd
}

// newSessionListCmd 创建 otter list 命令。
func newSessionListCmd() *cobra.Command {
	var showAll bool

	cmd := &cobra.Command{
		Use:   "list [provider]",
		Short: "列举会话",
		Long: `列出指定 provider 的所有会话。

不加 provider 参数时，列出所有 provider 的会话（按最后活跃时间排序）。

示例:
  otter session list deepseek
  otter session list --all
  otter session list`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionMgr := session.NewSessionManager(dataDir)

			var sessions []*session.Session
			var err error

			if len(args) > 0 && !showAll {
				provider := resolveProvider(args[0])
				sessions, err = sessionMgr.List(provider)
			} else {
				sessions, err = sessionMgr.ListAll()
			}
			if err != nil {
				return fmt.Errorf("列举会话失败: %w", err)
			}

			if len(sessions) == 0 {
				fmt.Println("暂无会话。使用 `otter session new <provider>` 创建新会话。")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "会话 ID\t标题\tProvider\t消息数\t最后活跃")
			fmt.Fprintln(w, "--------\t----\t--------\t------\t--------")

			for _, s := range sessions {
				updated := s.UpdatedAt.Format(time.RFC3339)
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n",
					s.ID, s.Title, s.Provider, s.MessageCount, updated)
			}
			w.Flush()

			return nil
		},
	}

	cmd.Flags().BoolVar(&showAll, "all", false, "列出所有 provider 的会话")

	return cmd
}

// newSessionShowCmd 创建 otter show 命令。
func newSessionShowCmd() *cobra.Command {
	var showMessages bool

	cmd := &cobra.Command{
		Use:   "show <session-id>",
		Short: "查看会话详情",
		Long: `查看指定会话的元数据和消息历史。

示例:
  otter session show sess_abc123
  otter session show sess_abc123 --messages`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]

			sessionMgr := session.NewSessionManager(dataDir)

			sess, err := sessionMgr.Get(sessionID)
			if err != nil {
				return fmt.Errorf("获取会话失败: %w", err)
			}
			if sess == nil {
				return fmt.Errorf("会话 %s 不存在", sessionID)
			}

			fmt.Printf("会话 ID:   %s\n", sess.ID)
			fmt.Printf("Provider:  %s\n", sess.Provider)
			fmt.Printf("标题:      %s\n", sess.Title)
			fmt.Printf("创建时间:  %s\n", sess.CreatedAt.Format(time.RFC3339))
			fmt.Printf("最后活跃:  %s\n", sess.UpdatedAt.Format(time.RFC3339))
			fmt.Printf("消息数:    %d\n", sess.MessageCount)

			if showMessages {
				msgs, err := sessionMgr.GetMessages(sessionID)
				if err != nil {
					return fmt.Errorf("获取消息历史失败: %w", err)
				}

				fmt.Println("\n--- 消息历史 ---")
				for i, msg := range msgs {
					role := "👤 User"
					if msg.Role == "assistant" {
						role = "🤖 Assistant"
					}
					preview := msg.Content
					if len(preview) > 200 {
						preview = preview[:200] + "..."
					}
					fmt.Printf("\n[%d] %s (%s):\n", i+1, role, msg.CreatedAt.Format(time.RFC3339))
					fmt.Printf("    %s\n", preview)
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&showMessages, "messages", false, "显示消息历史")

	return cmd
}

// newSessionRMCmd 创建 otter rm 命令。
func newSessionRMCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <session-id>",
		Short: "删除会话",
		Long: `删除指定会话及其所有消息记录。

示例:
  otter session rm sess_abc123`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]

			sessionMgr := session.NewSessionManager(dataDir)

			if err := sessionMgr.Delete(sessionID); err != nil {
				return fmt.Errorf("删除会话失败: %w", err)
			}

			fmt.Printf("✅ 已删除会话 %s\n", sessionID)
			return nil
		},
	}

	return cmd
}

// resolveProvider 将用户输入的别名解析为标准 provider 名称。
func resolveProvider(input string) string {
	switch input {
	case "gpt", "chatgpt":
		return "chatgpt"
	case "gem", "gemini":
		return "gemini"
	case "dep", "deepseek":
		return "deepseek"
	default:
		return input
	}
}
