// Package output 提供输出格式选择与终端渲染。
package output

import (
	"fmt"
	"os"
	"strings"
)

// OutputType 定义输出格式。
type OutputType string

const (
	Text     OutputType = "text"
	Markdown OutputType = "markdown"
	Raw      OutputType = "raw"
)

// ParseOutputType 解析输出格式字符串。
func ParseOutputType(s string) (OutputType, error) {
	switch strings.ToLower(s) {
	case "text", "":
		return Text, nil
	case "markdown", "md":
		return Markdown, nil
	case "raw":
		return Raw, nil
	default:
		return Text, fmt.Errorf("不支持的输出格式: %s（支持: text, markdown, raw）", s)
	}
}

// Format 根据指定的输出格式渲染内容。
func Format(content string, outputType OutputType) string {
	switch outputType {
	case Text:
		return stripHTML(content)
	case Markdown:
		return content
	case Raw:
		return content
	default:
		return content
	}
}

// Write 将格式化后的内容写入目标（stdout 或文件）。
func Write(content string, outputType OutputType, filePath string, appendMode bool) error {
	formatted := Format(content, outputType)

	if filePath == "" {
		// 输出到 stdout
		fmt.Print(formatted)
		if !strings.HasSuffix(formatted, "\n") {
			fmt.Println()
		}
		return nil
	}

	// 输出到文件
	flags := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}

	f, err := os.OpenFile(filePath, flags, 0644)
	if err != nil {
		return fmt.Errorf("打开输出文件失败: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(formatted); err != nil {
		return fmt.Errorf("写入输出文件失败: %w", err)
	}

	return nil
}

// stripHTML 简单移除 HTML 标签，用于纯文本输出。
func stripHTML(html string) string {
	var sb strings.Builder
	inTag := false
	for _, r := range html {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			sb.WriteRune(r)
		}
	}

	// 解码常见 HTML 实体
	result := sb.String()
	result = strings.ReplaceAll(result, "&amp;", "&")
	result = strings.ReplaceAll(result, "&lt;", "<")
	result = strings.ReplaceAll(result, "&gt;", ">")
	result = strings.ReplaceAll(result, "&quot;", "\"")
	result = strings.ReplaceAll(result, "&#39;", "'")
	result = strings.ReplaceAll(result, "&nbsp;", " ")

	// 合并多余空行
	lines := strings.Split(result, "\n")
	var cleaned []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			cleaned = append(cleaned, trimmed)
		} else if len(cleaned) > 0 && cleaned[len(cleaned)-1] != "" {
			cleaned = append(cleaned, "")
		}
	}

	return strings.Join(cleaned, "\n")
}
