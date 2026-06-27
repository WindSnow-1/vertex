package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

func splitIntoRuneChunks(text string) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	chunkSize := 1
	if cs := len(runes) / 8; cs > 1 {
		chunkSize = cs
	}
	chunks := make([]string, 0, (len(runes)+chunkSize-1)/chunkSize)
	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}

type sseWriter struct {
	w     http.ResponseWriter
	flush func()
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := &sseWriter{w: w}
	if flusher != nil {
		sw.flush = flusher.Flush
	}
	return sw
}

func (sw *sseWriter) write(line string) bool {
	if _, err := sw.w.Write([]byte(line)); err != nil {
		return false
	}
	if sw.flush != nil {
		sw.flush()
	}
	return true
}

func (c *Client) oaiFakeStream(ctx context.Context, w http.ResponseWriter, model string, geminiPayload map[string]any) {
	requestID := generateID()
	sw := newSSEWriter(w)

	cfg := c.cfg
	op := func(ctx context.Context, proxyURI string) (map[string]any, error) {
		return c.completeInner(ctx, model, geminiPayload, proxyURI)
	}

	resp, err := runParallel(ctx, cfg, op)
	if err != nil {
		ve := asVertexError(err)
		if ve == nil {
			ve = newInternalError(err.Error())
		}
		errJSON, _ := json.Marshal(formatOAIError(ve.Code, ve.Message, "api_error"))
		sw.write("data: " + string(errJSON) + "\n\n")
		sw.write("data: [DONE]\n\n")
		return
	}

	oai := geminiToOAI(resp, model)
	contentText := firstChoiceContent(oai)

	createdTS := time.Now().Unix()
	chunks := splitIntoRuneChunks(contentText)
	for i, piece := range chunks {
		base := map[string]any{
			"id":      "chatcmpl-" + requestID,
			"object":  "chat.completion.chunk",
			"created": createdTS,
			"model":   model,
		}
		var delta map[string]any
		if i == 0 {
			delta = map[string]any{"role": "assistant", "content": piece}
		} else {
			delta = map[string]any{"content": piece}
		}
		choice := map[string]any{"index": 0, "delta": delta}
		if i == len(chunks)-1 {
			choice["finish_reason"] = "stop"
		}
		base["choices"] = []any{choice}
		if !sw.write(sseLine(base)) {
			return
		}
	}

	if len(chunks) == 0 {
		base := map[string]any{
			"id":      "chatcmpl-" + requestID,
			"object":  "chat.completion.chunk",
			"created": createdTS,
			"model":   model,
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": ""},
				"finish_reason": "stop",
			}},
		}
		sw.write(sseLine(base))
	}

	sw.write("data: [DONE]\n\n")
}

func firstChoiceContent(oai map[string]any) string {
	choices, ok := oai["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		return ""
	}
	if c, ok := msg["content"].(string); ok {
		return c
	}
	return ""
}
