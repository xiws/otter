package api

import (
	"context"
	"os"
	"testing"
	"time"

	"otter/pkg/chrome"
)

// 手动实测（需 macOS + Chrome 已登录 + 设置 OTTER_LIVE_TEST=1）：
//
//	OTTER_LIVE_TEST=1 go test ./pkg/api/ -run TestGeminiLiveManual -v
//
// 会向账号真实发送 2 条消息（单轮 + 多轮）。
func TestGeminiLiveManual(t *testing.T) {
	if os.Getenv("OTTER_LIVE_TEST") == "" {
		t.Skip("设置 OTTER_LIVE_TEST=1 开启实况测试")
	}

	cookies, err := chrome.ExtractCookies("google.com")
	if err != nil {
		t.Fatalf("提取 cookie 失败: %v", err)
	}
	t.Logf("提取到 %d 个 Google cookie", len(cookies))

	api := NewGeminiAPI()
	api.SetCookies(cookies)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if err := api.Init(ctx); err != nil {
		t.Fatalf("Init 失败: %v", err)
	}
	at := api.AccessToken()
	if len(at) > 12 {
		at = at[:12]
	}
	t.Logf("Init 成功: at=%s... bl=%s sid=%s", at, api.bl, api.sid)

	var lastMeta []any
	full, meta := runGeminiTurn(t, ctx, api, "请只回复这句话：你好，我是 Gemini 测试。", nil)
	t.Logf("第 1 轮回复: %s", full)
	lastMeta = meta

	full2, _ := runGeminiTurn(t, ctx, api, "我刚才让你回复的那句话是什么？", lastMeta)
	t.Logf("第 2 轮回复: %s", full2)
}

// runGeminiTurn 执行一轮对话并返回完整文本与最后 metadata。
func runGeminiTurn(t *testing.T, ctx context.Context, api *GeminiAPI, prompt string, metadata []any) (string, []any) {
	t.Helper()
	ch, err := api.StreamGenerate(ctx, prompt, metadata)
	if err != nil {
		t.Fatalf("StreamGenerate 失败: %v", err)
	}
	var full string
	var lastMeta []any
	var cid, rid string
	for chunk := range ch {
		switch chunk.Type {
		case "text":
			full += chunk.Content
		case "meta":
			if chunk.ConversationID != "" {
				cid = chunk.ConversationID
			}
			if chunk.ResponseID != "" {
				rid = chunk.ResponseID
			}
			if chunk.Metadata != nil {
				lastMeta = chunk.Metadata
			}
		case "done":
		case "error":
			t.Fatalf("流式错误: %v", chunk.Err)
		}
	}
	t.Logf("  [meta] cid=%s rid=%s meta=%v", cid, rid, lastMeta)
	if full == "" {
		t.Fatalf("回复为空")
	}
	return full, lastMeta
}

// ChatGPT 手动实测（需 macOS + Chrome 已登录 chatgpt.com + 设置 OTTER_LIVE_TEST=1）：
//
//	OTTER_LIVE_TEST=1 go test ./pkg/api/ -run TestChatGPTLiveManual -v
//
// 会向账号真实发送 2 条消息（单轮 + 多轮）。
func TestChatGPTLiveManual(t *testing.T) {
	if os.Getenv("OTTER_LIVE_TEST") == "" {
		t.Skip("设置 OTTER_LIVE_TEST=1 开启实况测试")
	}

	cookies, err := chrome.ExtractCookies("chatgpt.com", "openai.com")
	if err != nil {
		t.Fatalf("提取 cookie 失败: %v", err)
	}
	t.Logf("提取到 %d 个 ChatGPT cookie", len(cookies))

	api := NewChatGPTAPI()
	api.SetCookies(cookies)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	session, err := api.Session(ctx)
	if err != nil {
		t.Fatalf("Session 失败: %v", err)
	}
	if session.User != nil {
		t.Logf("登录账号: %s", session.User.Email)
	}
	t.Logf("accessToken: %d 字符", len(session.AccessToken))

	if slugs, err := api.GetModels(ctx); err == nil {
		t.Logf("可用模型: %v", slugs)
	} else {
		t.Logf("获取模型列表失败（不阻断）: %v", err)
	}

	full, convID, msgID := runChatGPTTurn(t, ctx, api, "请只回复这句话：你好，我是 ChatGPT 测试。", "", "")
	t.Logf("第 1 轮回复: %s", full)

	full2, convID2, _ := runChatGPTTurn(t, ctx, api, "我刚才让你回复的那句话是什么？", convID, msgID)
	t.Logf("第 2 轮回复: %s", full2)
	if convID2 != convID {
		t.Errorf("多轮会话 ID 不一致: %s vs %s", convID2, convID)
	}
}

// runChatGPTTurn 执行一轮对话并返回完整文本、会话 ID 与回复消息 ID。
func runChatGPTTurn(t *testing.T, ctx context.Context, api *ChatGPTAPI, prompt, convID, parentID string) (string, string, string) {
	t.Helper()
	ch, err := api.Send(ctx, prompt, convID, parentID)
	if err != nil {
		t.Fatalf("Send 失败: %v", err)
	}

	var full string
	var gotConv, gotMsg string
	for chunk := range ch {
		switch chunk.Type {
		case "text":
			full += chunk.Content
		case "meta", "done":
			if chunk.ConversationID != "" {
				gotConv = chunk.ConversationID
			}
			if chunk.MessageID != "" {
				gotMsg = chunk.MessageID
			}
		case "error":
			t.Fatalf("流式错误: %v", chunk.Err)
		}
	}
	t.Logf("  [meta] conv=%s msg=%s", gotConv, gotMsg)
	if full == "" {
		t.Fatalf("回复为空")
	}
	return full, gotConv, gotMsg
}
