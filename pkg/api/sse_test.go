package api

import (
	"context"
	"strings"
	"testing"
)

// collectSSE 跑一遍解析器并汇总结果。
func collectSSE(t *testing.T, stream string) (text string, respID, tokenUsage int, title string, errMsg string, sawDone bool) {
	t.Helper()

	ch := make(chan StreamChunk, 64)
	parseSSE(context.Background(), strings.NewReader(stream), ch)
	close(ch)

	var sb strings.Builder
	for c := range ch {
		switch c.Type {
		case "text":
			sb.WriteString(c.Content)
		case "meta":
			if c.ResponseMessageID != 0 {
				respID = c.ResponseMessageID
			}
			if c.TokenUsage != 0 {
				tokenUsage = c.TokenUsage
			}
			if c.Title != "" {
				title = c.Title
			}
		case "done":
			sawDone = true
		case "error":
			errMsg = c.Content
		}
	}
	return sb.String(), respID, tokenUsage, title, errMsg, sawDone
}

// 长回复：初始帧 content 为空，正文靠增量帧累加；
// 增量帧有两种形态，都必须收集。
func TestParseSSELongReply(t *testing.T) {
	const stream = `event: ready
data: {"request_message_id":1,"response_message_id":2,"model_type":"default"}

event: update_session
data: {"updated_at":1789202215.154007}

data: {"v":{"response":{"message_id":2,"parent_id":1,"role":"ASSISTANT","status":"WIP","content":""}}}

data: {"p":"response/content","o":"APPEND","v":"你"}

data: {"v":"好"}

data: {"p":"response/accumulated_token_usage","o":"SET","v":42}

data: {"p":"response/status","v":"FINISHED"}

event: finish
data: {}

event: title
data: {"content":"问候"}

event: close
data: {"click_behavior":"none","auto_resume":false}
`
	text, respID, tokens, title, errMsg, done := collectSSE(t, stream)
	if text != "你好" {
		t.Errorf("正文 = %q, 期望 %q", text, "你好")
	}
	if respID != 2 {
		t.Errorf("ResponseMessageID = %d, 期望 2", respID)
	}
	if tokens != 42 {
		t.Errorf("TokenUsage = %d, 期望 42", tokens)
	}
	if title != "问候" {
		t.Errorf("Title = %q, 期望 %q", title, "问候")
	}
	if errMsg != "" {
		t.Errorf("不应有错误, 得到 %q", errMsg)
	}
	if !done {
		t.Error("缺少 done 事件")
	}
}

// 短回复：服务端一次生成完毕，不再发增量帧，
// 内容只存在于初始帧的 content 快照里。
func TestParseSSEShortReplySnapshot(t *testing.T) {
	const stream = `data: {"request_message_id":15,"response_message_id":16,"model_type":"default"}

data: {"v":{"response":{"message_id":16,"role":"ASSISTANT","status":"WIP","content":"4"}}}

data: {"p":"response/accumulated_token_usage","v":641}

data: {"p":"response/status","v":"FINISHED"}
`
	text, respID, tokens, _, errMsg, _ := collectSSE(t, stream)
	if text != "4" {
		t.Errorf("正文 = %q, 期望 %q（快照内容不能丢）", text, "4")
	}
	if respID != 16 {
		t.Errorf("ResponseMessageID = %d, 期望 16", respID)
	}
	if tokens != 641 {
		t.Errorf("TokenUsage = %d, 期望 641", tokens)
	}
	if errMsg != "" {
		t.Errorf("不应有错误, 得到 %q", errMsg)
	}
}

// HTTP 200 下的业务错误信封必须上报，否则调用方只看到空回复。
func TestParseSSEBizError(t *testing.T) {
	const stream = `data: {"code":0,"msg":"","data":{"biz_code":9,"biz_msg":"invalid ref file id","biz_data":null}}
`
	_, _, _, _, errMsg, _ := collectSSE(t, stream)
	if errMsg != "invalid ref file id" {
		t.Errorf("错误信息 = %q, 期望 %q", errMsg, "invalid ref file id")
	}
}

// 服务端状态帧不应被当成正文。
func TestParseSSEIgnoresStatusAsText(t *testing.T) {
	const stream = `data: {"p":"response/status","v":"FINISHED"}
`
	text, _, _, _, _, done := collectSSE(t, stream)
	if text != "" {
		t.Errorf("正文 = %q, 期望空（FINISHED 不是正文）", text)
	}
	if !done {
		t.Error("FINISHED 应当产生 done 事件")
	}
}
