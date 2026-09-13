package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"otter/pkg/config"
	"otter/pkg/output"
	"otter/pkg/provider"
	"otter/pkg/session"
)

// sendFlags 是发送消息子命令的通用 flags。
type sendFlags struct {
	files      []string
	outputFmt  string
	sessionID  string
	timeout    time.Duration
	noStream   bool
	browser    bool
	outputFile string
	appendMode bool
}

type providerContextConfig struct {
	providerName   string
	authToken      string
	defaultTimeout time.Duration
}

// newSendCmd 创建一个通用发送命令的构建器。
func newSendCmd(use, short, long string, pcc *providerContextConfig) *cobra.Command {
	sf := &sendFlags{}

	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long:  long,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := args[0]
			return runSend(cmd, sf, pcc, prompt)
		},
	}

	cmd.Flags().StringArrayVarP(&sf.files, "file", "f", []string{}, "附件路径（可重复）")
	cmd.Flags().StringVarP(&sf.outputFmt, "type", "t", "text", "输出格式: text, markdown, raw")
	cmd.Flags().StringVarP(&sf.sessionID, "session", "s", "", "指定会话 ID（覆盖活动会话）")
	cmd.Flags().DurationVar(&sf.timeout, "timeout", 5*time.Minute, "请求超时")
	cmd.Flags().BoolVar(&sf.noStream, "no-stream", false, "关闭流式输出")
	cmd.Flags().BoolVarP(&sf.browser, "browser", "b", false, "强制使用浏览器模式")
	cmd.Flags().StringVarP(&sf.outputFile, "output", "o", "", "输出到文件")
	cmd.Flags().BoolVar(&sf.appendMode, "append", false, "追加到文件（需配合 -o）")

	return cmd
}

// runSend 是发送命令的通用执行逻辑。
func runSend(cmd *cobra.Command, sf *sendFlags, pcc *providerContextConfig, prompt string) error {
	outType, err := output.ParseOutputType(sf.outputFmt)
	if err != nil {
		return err
	}

	sessionMgr := session.NewSessionManager(dataDir)

	sessID := sf.sessionID
	if sessID == "" {
		activeSess, err := sessionMgr.ActiveSession(pcc.providerName)
		if err != nil {
			return fmt.Errorf("获取活动会话失败: %w", err)
		}
		if activeSess != nil {
			sessID = activeSess.ID
			logDebug("使用活动会话: %s (%s)", sessID, activeSess.Title)
		}
	}

	var prov provider.Provider
	switch pcc.providerName {
	case "deepseek":
		ds := provider.NewDeepSeekProvider()
		if err := initDeepSeekProvider(ds); err != nil {
			return err
		}
		prov = ds
	case "chatgpt":
		cg := provider.NewChatGPTProvider()
		if err := initChatGPTProvider(cg); err != nil {
			return err
		}
		prov = cg
	case "gemini":
		gm := provider.NewGeminiProvider()
		if err := initGeminiProvider(gm); err != nil {
			return err
		}
		prov = gm
	default:
		return fmt.Errorf("不支持的 provider: %s", pcc.providerName)
	}

	var fileAttachments []provider.FileAttachment
	for _, fp := range sf.files {
		info, err := os.Stat(fp)
		if err != nil {
			return fmt.Errorf("文件 %s 无法访问: %w", fp, err)
		}
		fileAttachments = append(fileAttachments, provider.FileAttachment{
			Path: fp,
			Size: info.Size(),
		})
		logDebug("添加附件: %s (%d bytes)", fp, info.Size())
	}

	if sessID == "" {
		newSess, err := sessionMgr.Create(pcc.providerName, "")
		if err != nil {
			return fmt.Errorf("创建会话失败: %w", err)
		}
		sessID = newSess.ID
		logDebug("创建新会话: %s", sessID)
		if err := sessionMgr.SetActive(sessID); err != nil {
			logDebug("设置活动会话失败: %v", err)
		}
	}

	sendReq := &provider.SendRequest{
		Prompt:    prompt,
		Files:     fileAttachments,
		SessionID: sessID,
		Timeout:   sf.timeout,
	}

	// DeepSeek 这类平台的会话 ID 由服务端生成：本地会话需要先换取
	// 平台会话 ID，多轮追问还要带上上一轮的回复 ID。
	if err := prepareRemoteSession(cmd.Context(), prov, sessionMgr, sessID, sendReq); err != nil {
		return err
	}
	logDebug("发送请求: chat_session=%s parent_message_id=%d parent_message_id_str=%s 附件=%d 个",
		sendReq.ChatSessionID, sendReq.ParentMessageID, sendReq.ParentMessageIDStr, len(sendReq.Files))

	if !sf.noStream && (pcc.providerName == "deepseek" || pcc.providerName == "chatgpt" || pcc.providerName == "gemini") {
		return streamOutput(cmd, prov, sendReq, outType, sf, sessionMgr, sessID)
	}

	resp, err := prov.Send(cmd.Context(), sendReq)
	if err != nil {
		return fmt.Errorf("发送消息失败: %w", err)
	}

	persistRemoteReply(sessionMgr, sessID, resp)

	userMsg := &session.Message{
		Role:    "user",
		Content: prompt,
		Files:   sf.files,
	}
	_ = sessionMgr.AddMessage(sessID, userMsg)

	assistantMsg := &session.Message{
		Role:      "assistant",
		Content:   resp.Content,
		TokenUsed: resp.TokenUsed,
	}
	_ = sessionMgr.AddMessage(sessID, assistantMsg)

	if err := output.Write(resp.Content, outType, sf.outputFile, sf.appendMode); err != nil {
		return err
	}

	return nil
}

// prepareRemoteSession 为需要平台侧会话的 provider 补齐会话标识。
//
// ChatGPT/Gemini 的会话由首轮消息在平台侧自动创建：直接透传已保存的
// 会话 ID、父消息 ID 与续聊元数据；DeepSeek 则需先换取平台会话 ID，
// 追问时把上一轮回复 ID（整数）作为父消息 ID。
func prepareRemoteSession(ctx context.Context, prov provider.Provider, mgr *session.SessionManager, sessID string, req *provider.SendRequest) error {
	sess, err := mgr.Get(sessID)
	if err != nil {
		return fmt.Errorf("读取会话失败: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("会话 %s 不存在", sessID)
	}

	switch prov.Name() {
	case "chatgpt", "gemini":
		req.ChatSessionID = sess.ExternalID
		req.ParentMessageIDStr = sess.Metadata["parent_message_id"]
		if len(sess.Metadata) > 0 {
			req.RemoteMetadata = sess.Metadata
		}
		return nil
	}

	rsp, ok := prov.(provider.RemoteSessionProvider)
	if !ok {
		return nil
	}

	remoteID := sess.ExternalID
	if remoteID == "" {
		remoteID, err = rsp.CreateRemoteSession(ctx)
		if err != nil {
			return fmt.Errorf("创建平台会话失败: %w", err)
		}
		if err := mgr.UpdateRemote(sessID, remoteID, nil); err != nil {
			logDebug("保存平台会话 ID 失败: %v", err)
		}
		logDebug("已创建平台会话: %s", remoteID)
	}
	req.ChatSessionID = remoteID

	if pid := sess.Metadata["parent_message_id"]; pid != "" {
		if n, err := strconv.Atoi(pid); err == nil {
			req.ParentMessageID = n
		}
	}
	return nil
}

// persistRemoteReply 记录本轮回复的会话标识与元数据，供下一轮续聊使用。
//
// 平台会话 ID 写入 ExternalID；父消息 ID（整数或字符串）与平台续聊
// 元数据（如 Gemini 的 conversation metadata）合并进 Metadata。
func persistRemoteReply(mgr *session.SessionManager, sessID string, resp *provider.SendResponse) {
	meta := make(map[string]string)
	if resp.ResponseMessageID > 0 {
		meta["parent_message_id"] = strconv.Itoa(resp.ResponseMessageID)
	}
	if resp.ResponseMessageIDStr != "" {
		meta["parent_message_id"] = resp.ResponseMessageIDStr
	}
	for k, v := range resp.RemoteMetadata {
		if v != "" {
			meta[k] = v
		}
	}
	if resp.RemoteConversationID == "" && len(meta) == 0 {
		return
	}
	if err := mgr.UpdateRemote(sessID, resp.RemoteConversationID, meta); err != nil {
		logDebug("保存远程会话标识失败: %v", err)
	}
}

// initDeepSeekProvider 初始化 DeepSeek Provider，支持自动登录。
// 优先级：环境变量 token > 配置文件 token > 账号密码自动登录。
func initDeepSeekProvider(ds *provider.DeepSeekProvider) error {
	if token := os.Getenv("OTTER_DEEPSEEK_AUTH_TOKEN"); token != "" {
		ds.SetAuthToken(token)
		return nil
	}
	if appConfig.DeepSeek.AuthToken != "" {
		ds.SetAuthToken(appConfig.DeepSeek.AuthToken)
		return nil
	}
	account := appConfig.DeepSeek.Account
	password := config.GetPassword(&appConfig.DeepSeek, "OTTER_DEEPSEEK_PASSWORD")
	if account != "" && password != "" {
		ds.SetAccount(account)
		ds.SetPassword(password)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := ds.Login(ctx); err != nil {
			return fmt.Errorf("自动登录失败: %w", err)
		}
		if token := ds.Token(); token != "" {
			appConfig.DeepSeek.AuthToken = token
			_ = config.Save(appConfig, cfgFile)
		}
		return nil
	}
	return fmt.Errorf("DeepSeek 未配置。请执行:\n  otter config set deepseek.account <账号>\n  otter config set deepseek.password <密码>")
}

// initChatGPTProvider 初始化 ChatGPT Provider。
//
// 凭据解析（优先级从高到低，由 provider 内部链式处理）：
//  1. cookie 持久化文件（登录后自动保存）
//  2. 从本机 Chrome 自动反解 chatgpt.com cookie
//  3. 手动凭据兼容通路：环境变量/配置文件的 access_token / session_token
func initChatGPTProvider(cg *provider.ChatGPTProvider) error {
	cg.SetCookiesPath(config.ResolvePath(appConfig.ChatGPT.CookiesPath))
	if appConfig.ChatGPT.Model != "" {
		cg.SetModel(appConfig.ChatGPT.Model)
	}

	// 手动凭据兼容通路（优先级低于完整 cookie）
	switch {
	case os.Getenv("OTTER_CHATGPT_ACCESS_TOKEN") != "":
		cg.SetAccessToken(os.Getenv("OTTER_CHATGPT_ACCESS_TOKEN"))
	case os.Getenv("OTTER_CHATGPT_SESSION_TOKEN") != "":
		cg.SetSessionToken(os.Getenv("OTTER_CHATGPT_SESSION_TOKEN"))
	case appConfig.ChatGPT.AuthToken != "":
		cg.SetAccessToken(appConfig.ChatGPT.AuthToken)
	case appConfig.ChatGPT.SessionToken != "":
		cg.SetSessionToken(appConfig.ChatGPT.SessionToken)
	}

	// 其余情况由 provider 自动从本机 Chrome 提取 cookie；
	// 若全部失败会给出 otter auth login gpt 指引
	return nil
}

// initGeminiProvider 初始化 Gemini Provider。
//
// 凭据解析（优先级从高到低，由 provider 内部链式处理）：
//  1. cookie 持久化文件（登录后自动保存）
//  2. 从本机 Chrome 自动反解 google.com cookie
func initGeminiProvider(gm *provider.GeminiProvider) error {
	gm.SetCookiesPath(config.ResolvePath(appConfig.Gemini.CookiesPath))
	if appConfig.Gemini.Language != "" {
		gm.SetLanguage(appConfig.Gemini.Language)
	}
	return nil
}

// streamOutput 处理流式输出。
func streamOutput(cmd *cobra.Command, prov provider.Provider, req *provider.SendRequest, outType output.OutputType, sf *sendFlags, sessionMgr *session.SessionManager, sessID string) error {
	ctx := cmd.Context()

	logDebug("开始流式接收")
	eventCh, err := prov.SendStream(ctx, req)
	if err != nil {
		return fmt.Errorf("启动流式输出失败: %w", err)
	}

	var fullContent string
	responseID := 0
	var responseIDStr, remoteConvID string
	var remoteMeta map[string]string
	tokenUsage := 0
	for event := range eventCh {
		switch event.Type {
		case "text":
			fullContent += event.Content
			if !quiet {
				fmt.Print(event.Content)
			}
		case "meta":
			if event.ResponseMessageID != 0 {
				responseID = event.ResponseMessageID
			}
			if event.ResponseMessageIDStr != "" {
				responseIDStr = event.ResponseMessageIDStr
			}
			if event.RemoteConversationID != "" {
				remoteConvID = event.RemoteConversationID
			}
			if len(event.RemoteMetadata) > 0 {
				remoteMeta = event.RemoteMetadata
			}
			if event.TokenUsage != 0 {
				tokenUsage = event.TokenUsage
			}
		case "done":
			if event.ResponseMessageID != 0 {
				responseID = event.ResponseMessageID
			}
			if event.ResponseMessageIDStr != "" {
				responseIDStr = event.ResponseMessageIDStr
			}
			if event.RemoteConversationID != "" {
				remoteConvID = event.RemoteConversationID
			}
			if len(event.RemoteMetadata) > 0 {
				remoteMeta = event.RemoteMetadata
			}
			if !quiet {
				fmt.Println()
			}
		case "error":
			if event.Err != nil {
				return fmt.Errorf("流式输出错误: %w", event.Err)
			}
			return fmt.Errorf("流式输出错误: %s", event.Content)
		}
	}

	persistRemoteReply(sessionMgr, sessID, &provider.SendResponse{
		ResponseMessageID:    responseID,
		ResponseMessageIDStr: responseIDStr,
		RemoteConversationID: remoteConvID,
		RemoteMetadata:       remoteMeta,
	})

	userMsg := &session.Message{
		Role:    "user",
		Content: req.Prompt,
		Files:   sf.files,
	}
	_ = sessionMgr.AddMessage(sessID, userMsg)

	assistantMsg := &session.Message{
		Role:      "assistant",
		Content:   fullContent,
		TokenUsed: tokenUsage,
	}
	_ = sessionMgr.AddMessage(sessID, assistantMsg)

	if sf.outputFile != "" {
		if err := output.Write(fullContent, outType, sf.outputFile, sf.appendMode); err != nil {
			return err
		}
	}

	return nil
}

// --- 具体命令 ---

func newGPTCmd() *cobra.Command {
	return newSendCmd("gpt", "发送消息到 ChatGPT", `向 ChatGPT (chatgpt.com) 发送消息。

凭据自动从本机 Chrome 反解（需已登录 chatgpt.com），Direct API 直连，支持流式输出与多轮会话。

示例:
  otter gpt "你好，世界"
  otter gpt "继续刚才的话题"`,
		&providerContextConfig{
			providerName:   "chatgpt",
			defaultTimeout: 5 * time.Minute,
		},
	)
}

func newGemCmd() *cobra.Command {
	return newSendCmd("gem", "发送消息到 Gemini", `向 Gemini (gemini.google.com) 发送消息。

凭据自动从本机 Chrome 反解（需已登录 gemini.google.com），Direct API 直连，支持流式输出与多轮会话。

示例:
  otter gem "你好，世界"
  otter gem "继续刚才的话题"`,
		&providerContextConfig{
			providerName:   "gemini",
			defaultTimeout: 5 * time.Minute,
		},
	)
}

func newDepCmd() *cobra.Command {
	return newSendCmd("dep", "发送消息到 DeepSeek", `向 DeepSeek (chat.deepseek.com) 发送消息。

支持账号密码直接登录（DeepSeek 自身认证），登录后自动保存 token 复用。

首次使用请先配置账号密码：
  otter config set deepseek.account <手机号/邮箱>
  otter config set deepseek.password <密码>
  otter dep "你好"     # 自动登录并发送

或手动登录：
  otter auth login deepseek

支持上传文件附件和流式输出。

示例:
  otter dep "快速问答"
  otter dep -f ./docs/code.py "解释这段代码"
  otter dep "写一首诗" --stream`,
		&providerContextConfig{
			providerName:   "deepseek",
			defaultTimeout: 5 * time.Minute,
		},
	)
}
