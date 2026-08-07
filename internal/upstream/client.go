// Package upstream 实现向 CNB /ai/chat/completions 发送请求并透传 SSE 流。
//
// 请求格式（逆向自前端 _app.js）：
//
//	POST /ai/chat/completions
//	Headers:
//	  Content-Type: application/json
//	  Csrftoken: <csrf token>          // 必须
//	  Cookie: csrfkey=<csrf key>        // 必须（与 token 配对）
//	  Origin/Referer: https://cnb.cool
//	Body:
//	  {
//	    "model": "deepseek-v4-flash",
//	    "stream": true,                 // 上游强制流式
//	    "messages": [...],
//	    "tools": [...],                 // 可选，NPC 自带工具
//	    "maxTokens": N,                 // 可选
//	    "enable_thinking": bool,        // 可选
//	    "presence_penalty": float       // 可选
//	  }
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cnb2api/internal/auth"
)

const (
	chatURL    = "https://cnb.cool/ai/chat/completions"
	userAgent  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"
)

// ChatRequest 是 CNB chat/completions 的请求体（OpenAI 兼容超集）。
type ChatRequest struct {
	Model           string         `json:"model"`
	Stream          bool           `json:"stream"`
	Messages        []ChatMessage  `json:"messages"`
	Tools           []Tool         `json:"tools,omitempty"`
	ToolChoice      any            `json:"tool_choice,omitempty"`
	MaxTokens       int            `json:"maxTokens,omitempty"`
	EnableThinking  *bool          `json:"enable_thinking,omitempty"`
	PresencePenalty *float64       `json:"presence_penalty,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
}

// ChatMessage 是聊天消息。
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Tool 是函数工具定义。
type Tool struct {
	Type     string         `json:"type"`
	Function ToolFunction   `json:"function"`
}

// ToolFunction 是工具函数定义。
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// SSEChunk 是解析后的 SSE 数据块。
type SSEChunk struct {
	Raw    []byte // 原始 data 内容
	IsDone bool   // 是否为 [DONE]
}

// Client 是 CNB 上游客户端。
type Client struct {
	hc       *http.Client
	csrfPool *auth.Pool
}

// NewClient 创建上游客户端。
func NewClient(pool *auth.Pool, timeout time.Duration) *Client {
	return &Client{
		hc: &http.Client{Timeout: timeout},
		csrfPool: pool,
	}
}

// Chat 向 CNB 发送聊天请求，返回 SSE 流。调用方负责关闭 resp.Body。
// 请求成功(2xx)时返回 resp，resp.Body 关闭时自动归还凭证；遇凭证失效(401/403)会尝试换凭证重试。
func (c *Client) Chat(ctx context.Context, req *ChatRequest) (*http.Response, error) {
	// 最多重试 3 次（换凭证）
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cs, err := c.csrfPool.Acquire()
		if err != nil {
			return nil, err
		}

		resp, err := c.doChat(ctx, cs, req)
		if err != nil {
			c.csrfPool.Report(cs, false)
			lastErr = err
			continue
		}
		switch resp.StatusCode {
		case http.StatusOK:
			// 成功：包装 body，关闭时自动归还凭证
			resp.Body = &reportCloser{ReadCloser: resp.Body, pool: c.csrfPool, cs: cs}
			return resp, nil
		case http.StatusUnauthorized, http.StatusForbidden:
			// 读取响应体判断是否为 CSRF 失效（可重试）还是业务拒绝（不可重试）
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			s := string(body)
			if isCSRFError(s) {
				// 凭证失效，标记并重试
				c.csrfPool.Report(cs, false)
				lastErr = fmt.Errorf("cnb: csrf rejected (status %d): %s", resp.StatusCode, strings.TrimSpace(s))
				continue
			}
			// 业务拒绝（如 Agent calls not allowed）：不重试，直接透传
			c.csrfPool.Report(cs, true) // 凭证本身有效，不消耗错误计数
			return nil, fmt.Errorf("cnb: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(s))
		default:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			lastErr = fmt.Errorf("cnb: upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			return nil, lastErr
		}
	}
	return nil, fmt.Errorf("cnb: chat failed after retries: %w", lastErr)
}

// isCSRFError 判断响应体是否为 CSRF 校验失败（区别于业务拒绝）。
func isCSRFError(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "csrf") ||
		strings.Contains(lower, "blocked by csrf") ||
		strings.Contains(lower, "csrf 校验失败")
}

// reportCloser 包装 resp.Body：关闭时自动归还凭证并上报结果。
type reportCloser struct {
	io.ReadCloser
	pool *auth.Pool
	cs   *auth.CSRF
}

func (r *reportCloser) Close() error {
	err := r.ReadCloser.Close()
	r.pool.Report(r.cs, err == nil)
	return err
}

// doChat 执行单次请求。
func (c *Client) doChat(ctx context.Context, cs *auth.CSRF, req *ChatRequest) (*http.Response, error) {
	// 强制流式（上游拒绝非流式）
	req.Stream = true

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, chatURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream, application/json, text/plain, */*")
	httpReq.Header.Set("User-Agent", userAgent)
	httpReq.Header.Set("Origin", "https://cnb.cool")
	httpReq.Header.Set("Referer", "https://cnb.cool/")
	httpReq.Header.Set("Csrftoken", cs.Token)
	httpReq.AddCookie(&http.Cookie{Name: "csrfkey", Value: cs.Key, Path: "/"})

	return c.hc.Do(httpReq)
}

// ReadSSE 从响应体读取 SSE 事件，逐行回调。返回是否正常结束。
// data 行的内容会解析出来；`data: [DONE]` 触发 IsDone。
func ReadSSE(resp *http.Response, onChunk func(chunk SSEChunk) error) error {
	defer resp.Body.Close()

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") &&
		resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("cnb: not sse (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024) // 支持大 chunk

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				return onChunk(SSEChunk{IsDone: true})
			}
			if err := onChunk(SSEChunk{Raw: []byte(payload)}); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
