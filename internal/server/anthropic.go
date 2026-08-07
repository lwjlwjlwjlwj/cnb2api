// Anthropic Messages API 适配层（/v1/messages、/anthropic/v1/messages）。
//
// 复用 cnb2api 现有管线：鉴权、CSRF 凭证池、上游请求、工具调用重塑。
// 本文件只做协议翻译：
//   - 输入：Anthropic 格式（system + messages + tools）→ 内部统一格式
//   - 输出：Anthropic 格式（非流式 message 对象 / 流式 SSE 事件序列）
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"cnb2api/internal/upstream"
)

// anthropicRequest 是 Anthropic Messages 请求格式（子集）。
type anthropicRequest struct {
	Model      string          `json:"model"`
	MaxTokens  int             `json:"max_tokens"`
	System     json.RawMessage `json:"system"` // string 或 [{type:text,...}]
	Messages   []anthropicMsg  `json:"messages"`
	Tools      []anthropicTool `json:"tools"`
	Stream     bool            `json:"stream"`
	Temperature *float64       `json:"temperature"`
	TopP        *float64       `json:"top_p"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string 或 [blocks]
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// handleAnthropicMessages 处理 POST /v1/messages。
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if !s.authAnthropic(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "invalid api key", "type": "auth_error"}})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}

	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "messages is required"})
		return
	}

	// 构造上游请求（OpenAI 兼容内部格式）
	upReq := &upstream.ChatRequest{
		Model:     s.model,
		Stream:    true,
		Messages:  make([]upstream.ChatMessage, 0, len(req.Messages)+1),
		MaxTokens: req.MaxTokens,
	}
	if req.Temperature != nil {
		upReq.Temperature = req.Temperature
	}
	if req.TopP != nil {
		upReq.TopP = req.TopP
	}

	// system 字段 → system message
	if len(req.System) > 0 && string(req.System) != "null" {
		if sys := extractAnthropicText(req.System); sys != "" {
			upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: "system", Content: sys})
		}
	}

	// 工具定义：不支持（纯文本透传），忽略 tools 参数

	// messages：Anthropic content 可能是 string 或 blocks
	for _, m := range req.Messages {
		content := extractAnthropicText(m.Content)
		role := m.Role
		if role == "assistant" && strings.HasPrefix(content, "<tool_use") {
			// Anthropic tool_result / tool_use 历史：简化转为文本
			content = m.Role + ": " + content
			role = "assistant"
		}
		upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: role, Content: content})
	}

	ctx := r.Context()
	resp, err := s.upstream.Chat(ctx, upReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		s.anthropicStreamResponse(w, resp)
	} else {
		s.anthropicNonStreamResponse(w, resp)
	}
}

// authAnthropic 校验 Anthropic 风格鉴权：x-api-key 头 或 Bearer。
func (s *Server) authAnthropic(r *http.Request) bool {
	if s.apiKey == "" {
		return true
	}
	// 优先 x-api-key（Claude Code 标准）
	if k := r.Header.Get("x-api-key"); k != "" {
		return k == s.apiKey
	}
	// 兼容 Bearer
	return s.auth(r)
}

// handleAnthropicCountTokens 处理 POST /v1/messages/count_tokens。
// 粗略估算 input token 数（与 qwen2api-rs 一致的 ×1.35 通胀系数）。
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []anthropicMsg  `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}
	var sb strings.Builder
	if len(req.System) > 0 && string(req.System) != "null" {
		sb.WriteString(extractAnthropicText(req.System))
		sb.WriteString("\n")
	}
	for _, m := range req.Messages {
		sb.WriteString(extractAnthropicText(m.Content))
		sb.WriteString("\n")
	}
	// 粗略估算：中文字符 ≈ 1 token，其他 ≈ 4 字符/token
	text := sb.String()
	cjk := 0
	for _, r := range text {
		if r > 0x2E80 {
			cjk++
		}
	}
	other := len(text) - cjk
	base := cjk + other/4
	inflated := int(float64(base) * 1.35)
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": inflated})
}

// extractAnthropicText 从 Anthropic content 提取纯文本（支持 string 或 blocks 数组）。
func extractAnthropicText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// 尝试 string
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// 尝试 blocks
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			switch b.Type {
			case "text":
				sb.WriteString(b.Text)
			case "tool_use":
				sb.WriteString("<tool_use name=\"" + b.Name + "\">")
			case "tool_result":
				sb.WriteString("(tool result)")
			}
			sb.WriteString("\n")
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

// ── 非流式 Anthropic message 响应 ──

func (s *Server) anthropicNonStreamResponse(w http.ResponseWriter, resp *http.Response) {
	var content strings.Builder
	_ = upstream.ReadSSE(resp, func(chunk upstream.SSEChunk) error {
		if chunk.IsDone {
			return nil
		}
		var obj struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk.Raw, &obj); err != nil {
			return nil
		}
		for _, c := range obj.Choices {
			content.WriteString(c.Delta.Content)
		}
		return nil
	})

	text := content.String()
	var contentBlocks []map[string]any
	if text != "" {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
	}
	if len(contentBlocks) == 0 {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": ""})
	}

	respObj := map[string]any{
		"id":            "msg_" + shortID(12),
		"type":          "message",
		"role":          "assistant",
		"model":         s.model,
		"content":       contentBlocks,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  0, // 上游 usage 未透传，置 0
			"output_tokens": 0,
		},
	}
	writeJSON(w, http.StatusOK, respObj)
}

// ── 流式 Anthropic SSE ──

func (s *Server) anthropicStreamResponse(w http.ResponseWriter, resp *http.Response) {
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

	msgID := "msg_" + shortID(12)

	// message_start
	start := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}
	s.anthropicEvent(w, flusher, start)

	// 边收边转发 text blocks
	blockOpen := false
	_ = upstream.ReadSSE(resp, func(chunk upstream.SSEChunk) error {
		if chunk.IsDone {
			return nil
		}
		var obj struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(chunk.Raw, &obj); err != nil {
			return nil
		}
		for _, c := range obj.Choices {
			text := c.Delta.Content
			if text == "" {
				continue
			}
			if !blockOpen {
				s.anthropicEvent(w, flusher, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
				blockOpen = true
			}
			s.anthropicEvent(w, flusher, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
		}
		flusher.Flush()
		return nil
	})
	if blockOpen {
		s.anthropicEvent(w, flusher, map[string]any{"type": "content_block_stop", "index": 0})
	}
	s.anthropicEvent(w, flusher, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 0}})
	s.anthropicEvent(w, flusher, map[string]any{"type": "message_stop"})
	flusher.Flush()
}

// anthropicEvent 输出一个 Anthropic SSE 事件。
func (s *Server) anthropicEvent(w http.ResponseWriter, flusher http.Flusher, v map[string]any) {
	data, _ := json.Marshal(v)
	w.Write([]byte("event: " + v["type"].(string) + "\n"))
	w.Write([]byte("data: " + string(data) + "\n\n"))
	flusher.Flush()
}
