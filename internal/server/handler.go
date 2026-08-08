// Package server 实现 OpenAI 兼容的 HTTP API:
//
//	GET  /v1/models            - 列出可用模型
//	POST /v1/chat/completions  - 聊天(流式/非流式)
//	GET  /healthz              - 健康检查
//	GET  /pool                 - CSRF 凭证池状态
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"cnb2api/internal/auth"
	"cnb2api/internal/upstream"
)

// Server 是 OpenAI 兼容 API 服务器。
type Server struct {
	pool     *auth.Pool
	upstream *upstream.Client
	apiKey   string
	model    string   // 默认模型
	models   []string // 支持的模型列表
}

// New 创建服务器。
func New(pool *auth.Pool, apiKey, model string, models []string, timeout time.Duration) *Server {
	if len(models) == 0 {
		models = []string{model}
	}
	return &Server{
		pool:     pool,
		upstream: upstream.NewClient(pool, timeout),
		apiKey:   apiKey,
		model:    model,
		models:   models,
	}
}

// Handler 返回 HTTP handler(可挂到任何路由)。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	// Anthropic Messages API
	mux.HandleFunc("/v1/messages", s.handleAnthropicMessages)
	mux.HandleFunc("/anthropic/v1/messages", s.handleAnthropicMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleAnthropicCountTokens)
	// OpenAI Responses API
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/pool", s.handlePool)
	return mux
}

// auth 校验 API key(配置为空则跳过鉴权)。
func (s *Server) auth(r *http.Request) bool {
	if s.apiKey == "" {
		return true
	}
	got := r.Header.Get("Authorization")
	got = strings.TrimPrefix(got, "Bearer ")
	got = strings.TrimSpace(got)
	return got == s.apiKey
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !s.auth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "auth_error"}})
		return
	}
	data := make([]map[string]any, 0, len(s.models))
	for _, m := range s.models {
		data = append(data, map[string]any{"id": m, "object": "model", "created": time.Now().Unix(), "owned_by": "cnb"})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// resolveModel 解析请求中的模型:优先用客户端指定的(须在白名单),否则用默认。
func (s *Server) resolveModel(reqModel string) string {
	if reqModel != "" {
		for _, m := range s.models {
			if m == reqModel {
				return m
			}
		}
	}
	return s.model
}

// chatRequest 是收到的 OpenAI 格式请求。
type chatRequest struct {
	Model       string            `json:"model"`
	Stream      bool              `json:"stream"`
	Messages    []chatMsg         `json:"messages"`
	MaxTokens   int               `json:"max_tokens"`
	Tools       []json.RawMessage `json:"tools"`
	Temperature *float64          `json:"temperature"`
	TopP        *float64          `json:"top_p"`
}

type chatMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// extractChatContent 兼容 content 为 string 或数组(多模态)两种格式,提取纯文本。
// 支持 OpenAI 标准(text/image_url)与 openclaw 自定义(thinking/toolCall/toolResult)格式。
func extractChatContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// 数组(OpenAI 多模态 / openclaw 结构化格式)
	var parts []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Thinking  string `json:"thinking"`
		Name      string `json:"name"`
		Arguments any    `json:"arguments"`
		// image_url 等其它类型忽略(CNB 不支持多模态)
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text":
				if p.Text != "" {
					sb.WriteString(p.Text)
				}
			case "thinking":
				if p.Thinking != "" {
					sb.WriteString(p.Thinking)
				}
			case "toolCall":
				// 工具调用信息转为文本(保持对话连续)
				args := ""
				if p.Arguments != nil {
					if b, err := json.Marshal(p.Arguments); err == nil {
						args = string(b)
					}
				}
				sb.WriteString(fmt.Sprintf("(assistant called tool %s with args %s)", p.Name, args))
			}
		}
		return sb.String()
	}
	return string(raw) // 兜底
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if !s.auth(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "auth_error"}})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20)) // 64MB 上限
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}

	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "messages is required"})
		return
	}

	// 请求摘要日志(用于诊断客户端问题)
	{
		totalChars := 0
		for _, m := range req.Messages {
			totalChars += len(extractChatContent(m.Content))
		}
		log.Printf("[REQ] model=%s stream=%v msgs=%d chars=%d tools=%d",
			req.Model, req.Stream, len(req.Messages), totalChars, len(req.Tools))
		// 诊断: 保存大请求体到 /tmp/cnb2api_last_req.json(仅当 msgs>100 或 chars>100000)
		if totalChars > 100000 || len(req.Messages) > 100 {
			if err := os.WriteFile("/tmp/cnb2api_last_req.json", body, 0o644); err == nil {
				log.Printf("[REQ] saved request body to /tmp/cnb2api_last_req.json (%d bytes)", len(body))
			}
		}
	}

	// 解析模型(客户端指定或默认)
	model := s.resolveModel(req.Model)

	// 构造上游请求
	upReq := &upstream.ChatRequest{
		Model:     model,
		Stream:    true, // 上游强制流式
		Messages:  make([]upstream.ChatMessage, 0, len(req.Messages)+1),
		MaxTokens: req.MaxTokens,
	}
	if req.Temperature != nil {
		upReq.Temperature = req.Temperature
	}
	if req.TopP != nil {
		upReq.TopP = req.TopP
	}

	// 消息转换:openclaw 自定义格式 → 上游可接受的 user/assistant 序列
	// 策略:
	//  1. toolResult/tool → user 消息,加 [工具执行结果] 前缀
	//  2. 连续多条 user(多个工具结果连发)→ 合并为一条,避免上游模型困惑
	//  3. 失败占位/空消息 → 过滤或替换为有意义内容
	//  4. assistant 空 content(工具调用后)→ (assistant called tools)
	//  5. 不做历史截断(用户要求保留完整上下文)
	msgsToProcess := req.Messages
	var converted []upstream.ChatMessage
	appendUser := func(content string) {
		if len(converted) > 0 && converted[len(converted)-1].Role == "user" {
			// 合并到上一条 user
			if converted[len(converted)-1].Content != "" {
				converted[len(converted)-1].Content += "\n\n"
			}
			converted[len(converted)-1].Content += content
		} else {
			converted = append(converted, upstream.ChatMessage{Role: "user", Content: content})
		}
	}
	for _, m := range msgsToProcess {
		content := extractChatContent(m.Content)
		role := m.Role
		// 过滤: 失败的 assistant 占位(无实际内容且无工具调用)
		if role == "assistant" && (strings.Contains(content, "[assistant turn failed") || strings.Contains(content, "turn failed before producing")) {
			continue
		}
		// openclaw 自定义角色 toolResult(工具执行结果),上游不认,转 user 并合并
		if role == "toolResult" {
			if content == "" {
				content = "(tool result)"
			}
			appendUser("[工具执行结果] " + content)
			continue
		}
		// openclaw 自定义角色 toolResult(工具执行结果),上游不认,转 user 并合并
		if role == "toolResult" {
			if content == "" {
				content = "(tool result)"
			}
			appendUser("[工具执行结果] " + content)
			continue
		}
		// 上游不认 tool 角色,转 user 并合并
		if role == "tool" {
			if content == "" {
				content = "(tool result)"
			}
			appendUser("[工具执行结果] " + content)
			continue
		}
		// user 消息:如果上一条是 user 也合并(连续 user)
		if role == "user" {
			appendUser(content)
			continue
		}
		// assistant 带 tool_calls 的消息:content 通常为 null,转成说明文本
		if role == "assistant" && content == "" {
			content = "(assistant called tools)"
		}
		// 合并后的 user 消息不允许紧跟另一条 user 时直接 append assistant
		converted = append(converted, upstream.ChatMessage{Role: role, Content: content})
	}
	// 清理空消息和尾部空 user
	var cleaned []upstream.ChatMessage
	for _, c := range converted {
		if c.Content == "" && c.Role == "user" {
			continue
		}

		cleaned = append(cleaned, c)
	}
	upReq.Messages = cleaned

	// 强制设置 maxTokens 以避免长上下文请求被上游截断输出
	// deepseek-v4 系列默认支持 65k 输出,设 60k 留余量
	upReq.MaxTokens = 60000

	// 转发前日志:消息转换结果摘要
	{
		roleCounts := map[string]int{}
		totalLen := 0
		for _, m := range upReq.Messages {
			roleCounts[m.Role]++
			totalLen += len(m.Content)
		}
		log.Printf("[FWD] model=%s msgs=%d totalChars=%d roles=%v lastRole=%s lastContentLen=%d maxTokens=%d",
			req.Model, len(upReq.Messages), totalLen, roleCounts,
			upReq.Messages[len(upReq.Messages)-1].Role,
			len(upReq.Messages[len(upReq.Messages)-1].Content), upReq.MaxTokens)
	}

	ctx := r.Context()
	resp, err := s.upstream.Chat(ctx, upReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		s.streamResponse(w, resp)
	} else {
		s.nonStreamResponse(w, resp)
	}
}

// streamResponse 透传 SSE 流,并转换成 OpenAI 流式格式。
func (s *Server) streamResponse(w http.ResponseWriter, resp *http.Response) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 纯文本透传:标准化转发上游 SSE chunk 为标准 OpenAI 格式
	var stdID, stdModel string
	var stdCreated int64
	var stdUsage map[string]any
	_ = upstream.ReadSSE(resp, func(chunk upstream.SSEChunk) error {
		if chunk.IsDone {
			// 收尾 chunk:finish_reason + usage + [DONE]
			if stdID != "" {
				final := map[string]any{
					"id": stdID, "model": stdModel, "created": stdCreated, "object": "chat.completion.chunk",
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				}
				if stdUsage != nil {
					final["usage"] = stdUsage
				}
				data, _ := json.Marshal(final)
				w.Write([]byte("data: " + string(data) + "\n\n"))
			}
			_, err := w.Write([]byte("data: [DONE]\n\n"))
			flusher.Flush()
			return err
		}
		var obj struct {
			ID      string         `json:"id"`
			Model   string         `json:"model"`
			Created int64          `json:"created"`
			Usage   map[string]any `json:"usage"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk.Raw, &obj); err != nil {
			return nil
		}
		if obj.ID != "" {
			stdID = obj.ID
		}
		if obj.Model != "" {
			stdModel = obj.Model
		}
		if obj.Created != 0 {
			stdCreated = obj.Created
		}
		if obj.Usage != nil {
			stdUsage = obj.Usage
		}
		for _, c := range obj.Choices {
			if c.Delta.Content != "" {
				chunkOut := map[string]any{
					"id": stdID, "model": stdModel, "created": stdCreated, "object": "chat.completion.chunk",
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": c.Delta.Content}}},
				}
				data, _ := json.Marshal(chunkOut)
				w.Write([]byte("data: " + string(data) + "\n\n"))
			}
			if c.Delta.Reasoning != "" {
				chunkOut := map[string]any{
					"id": stdID, "model": stdModel, "created": stdCreated, "object": "chat.completion.chunk",
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": c.Delta.Reasoning}}},
				}
				data, _ := json.Marshal(chunkOut)
				w.Write([]byte("data: " + string(data) + "\n\n"))
			}
		}
		flusher.Flush()
		return nil
	})
}

// nonStreamResponse 聚合 SSE 为单次 JSON 响应。
func (s *Server) nonStreamResponse(w http.ResponseWriter, resp *http.Response) {
	var (
		content     strings.Builder
		reasoning   strings.Builder
		lastID      string
		lastModel   string
		lastCreated int64
		lastFinish  string
		finalUsage  json.RawMessage
	)

	_ = upstream.ReadSSE(resp, func(chunk upstream.SSEChunk) error {
		if chunk.IsDone {
			return nil
		}
		var obj struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Created int64  `json:"created"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(chunk.Raw, &obj); err != nil {
			return nil // 跳过无法解析的块
		}
		if obj.ID != "" {
			lastID = obj.ID
		}
		if obj.Model != "" {
			lastModel = obj.Model
		}
		if obj.Created != 0 {
			lastCreated = obj.Created
		}
		for _, c := range obj.Choices {
			content.WriteString(c.Delta.Content)
			reasoning.WriteString(c.Delta.Reasoning)
			if c.FinishReason != nil {
				lastFinish = *c.FinishReason
			}
		}
		if len(obj.Usage) > 0 {
			finalUsage = obj.Usage
		}
		return nil
	})

	msg := map[string]any{
		"role":              "assistant",
		"content":           content.String(),
		"reasoning_content": reasoning.String(),
	}

	respObj := map[string]any{
		"id":      lastID,
		"object":  "chat.completion",
		"created": lastCreated,
		"model":   lastModel,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": lastFinish,
			},
		},
	}
	if len(finalUsage) > 0 {
		respObj["usage"] = json.RawMessage(finalUsage)
	}
	writeJSON(w, http.StatusOK, respObj)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"poolSize": s.pool.Count(),
	})
}

func (s *Server) handlePool(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"pool": s.pool.Stats(),
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

// shortID 生成短 ID（小写字母）。
func shortID(length int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, length)
	for i := range b {
		b[i] = letters[time.Now().UnixNano()%int64(len(letters))]
	}
	return string(b)
}
