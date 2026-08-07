// OpenAI Responses API 适配层（/v1/responses）。
//
// 接收 input/instructions 格式，转为 messages 后复用现有管线。
// 输出 Response 对象 + 流式事件（response.created/response.output_text.delta/...）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"cnb2api/internal/upstream"
)

// responsesRequest 是 OpenAI Responses 格式请求。
type responsesRequest struct {
	Model       string          `json:"model"`
	Input       json.RawMessage `json:"input"` // string 或 [message objects]
	Instructions string        `json:"instructions"`
	Tools       json.RawMessage `json:"tools"` // 保留字段但不使用
	Stream      bool           `json:"stream"`
	Reasoning   bool           `json:"reasoning"` // 启用思考
}

// handleResponses 处理 POST /v1/responses。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if !s.auth(r) {
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

	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}

	// 转换为内部格式
	upReq := &upstream.ChatRequest{
		Model:     s.model,
		Stream:    true, // 内部统一用流式
		Messages:  []upstream.ChatMessage{},
	}

	// instructions → system message
	if req.Instructions != "" {
		upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: "system", Content: req.Instructions})
	}

	// input → user messages
	inputText := extractInputText(req.Input)
	if inputText != "" {
		upReq.Messages = append(upReq.Messages, upstream.ChatMessage{Role: "user", Content: inputText})
	}

	ctx := r.Context()
	resp, err := s.upstream.Chat(ctx, upReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"message": err.Error(), "type": "upstream_error"}})
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		s.responsesStreamResponse(w, resp)
	} else {
		s.responsesNonStreamResponse(w, resp)
	}
}

// extractInputText 从 input 提取文本内容。
func extractInputText(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	// 尝试 string
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		return s
	}
	// 尝试 messages 数组
	var msgs []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &msgs); err == nil {
		var sb strings.Builder
		for _, m := range msgs {
			sb.WriteString(m.Role + ": " + m.Content + "\n")
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

// responsesNonStreamResponse 非流式响应：返回 Response 对象。
func (s *Server) responsesNonStreamResponse(w http.ResponseWriter, resp *http.Response) {
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
	var outputContent []map[string]any
	if text != "" {
		outputContent = append(outputContent, map[string]any{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		})
	}

	respID := "resp_" + shortID(24)
	respObj := map[string]any{
		"id":      respID,
		"object":  "response",
		"created_at": time.Now().Unix(),
		"model":   s.model,
		"status":  "completed",
		"output": []map[string]any{
			{
				"type":    "message",
				"id":      "msg_" + shortID(16),
				"role":    "assistant",
				"status":  "completed",
				"content": outputContent,
			},
		},
		"usage": map[string]any{
			"input_tokens":  0, // 未透传上游 usage
			"output_tokens": 0,
			"total_tokens":  0,
		},
	}
	writeJSON(w, http.StatusOK, respObj)
}

// responsesStreamResponse 流式响应：输出 SSE 事件序列。
func (s *Server) responsesStreamResponse(w http.ResponseWriter, resp *http.Response) {
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

	respID := "resp_" + shortID(24)

	// response.created 事件
	created := map[string]any{
		"type":     "response.created",
		"response": map[string]any{
			"id":     respID,
			"object": "response",
			"model":  s.model,
			"status": "in_progress",
		},
	}
	s.responsesEvent(w, flusher, "response.created", created)

	// 边收边输出 text delta
	fullText := ""
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
			if c.Delta.Content != "" {
				fullText += c.Delta.Content
				delta := map[string]any{
					"type":  "response.output_text.delta",
					"delta": c.Delta.Content,
				}
				s.responsesEvent(w, flusher, "response.output_text.delta", delta)
			}
		}
		return nil
	})

	// 完成事件（聚合所有输出）
	outputContent := []map[string]any{}
	if fullText != "" {
		outputContent = append(outputContent, map[string]any{
			"type":        "output_text",
			"text":        fullText,
			"annotations": []any{},
		})
	}
	completed := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":       respID,
			"object":   "response",
			"created_at": time.Now().Unix(),
			"model":    s.model,
			"status":   "completed",
			"output": []map[string]any{
				{
					"type":    "message",
					"id":      "msg_" + shortID(16),
					"role":    "assistant",
					"status":  "completed",
					"content": outputContent,
				},
			},
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
				"total_tokens":  0,
			},
		},
	}
	s.responsesEvent(w, flusher, "response.completed", completed)
	s.responsesEvent(w, flusher, "", map[string]any{"data": "[DONE]"})
	flusher.Flush()
}

// responsesEvent 输出一个 Responses SSE 事件。
func (s *Server) responsesEvent(w http.ResponseWriter, flusher http.Flusher, eventType string, v map[string]any) {
	if eventType != "" {
		w.Write([]byte("event: " + eventType + "\n"))
	}
	data, _ := json.Marshal(v)
	w.Write([]byte("data: " + string(data) + "\n\n"))
	flusher.Flush()
}
