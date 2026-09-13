// Package cli 提供 otter 的命令行界面实现。
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"otter/pkg/config"
)

var (
	cfgFile   string
	dataDir   string
	verbose   bool
	quiet     bool
	appConfig *config.Config
)

// NewRootCmd 创建 otter 的根命令。
func NewRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "otter",
		Short: "AI Web 批量自动化 CLI",
		Long: `otter 是一个命令行工具，通过浏览器自动化或直接 API 方式，
将本地文件与 Prompt 批量发送给 ChatGPT、Gemini、DeepSeek 等 AI 大模型。`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// 初始化配置
			if err := initConfig(); err != nil {
				return err
			}

			// 确保数据目录存在
			if err := config.EnsureDataDir(dataDir); err != nil {
				return fmt.Errorf("初始化数据目录失败: %w", err)
			}

			if verbose {
				fmt.Fprintf(os.Stderr, "配置路径: %s\n", cfgFile)
				fmt.Fprintf(os.Stderr, "数据目录: %s\n", dataDir)
			}

			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// 全局 flags
	cmd.PersistentFlags().StringVar(&cfgFile, "config", "", "配置文件路径 (默认 ~/.otter/config.json)")
	cmd.PersistentFlags().StringVar(&dataDir, "data-dir", "", "数据目录 (默认 ~/.otter/)")
	cmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "输出调试日志")
	cmd.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "仅输出核心结果")

	// 添加子命令
	cmd.AddCommand(newGPTCmd())
	cmd.AddCommand(newGemCmd())
	cmd.AddCommand(newDepCmd())
	cmd.AddCommand(newSessionCmd())
	cmd.AddCommand(newConfigCmd())
	cmd.AddCommand(newAuthCmd())

	return cmd
}

// initConfig 初始化配置。
func initConfig() error {
	if cfgFile == "" {
		cfgFile = config.ResolvePath("~/.otter/config.json")
	}
	if dataDir == "" {
		dataDir = config.ResolvePath("~/.otter/")
	}

	var err error
	appConfig, err = config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("加载配置失败: %w", err)
	}

	return nil
}

// logDebug 输出调试信息（仅 --verbose 时显示）。
func logDebug(format string, args ...interface{}) {
	if verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] "+format+"\n", args...)
	}
}

// logError 输出错误信息。
func logError(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[ERROR] "+format+"\n", args...)
}

// Execute 执行根命令。
func Execute() error {
	return NewRootCmd().Execute()
}
