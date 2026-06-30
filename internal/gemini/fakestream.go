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
	reasoningText := firstChoiceReasoning(oai)

	createdTS := time.Now().Unix()

	isFirst := true
	sendChunk := func(delta map[string]any, finish bool) bool {
		base := map[string]any{
			"id":      "chatcmpl-" + requestID,
			"object":  "chat.completion.chunk",
			"created": createdTS,
			"model":   model,
		}
		if isFirst {
			delta["role"] = "assistant"
			isFirst = false
		}
		choice := map[string]any{"index": 0, "delta": delta}
		if finish {
			choice["finish_reason"] = "stop"
		}
		base["choices"] = []any{choice}
		return sw.write(sseLine(base))
	}

	if reasoningText != "" {
		for _, piece := range splitIntoRuneChunks(reasoningText) {
			if !sendChunk(map[string]any{"reasoning_content": piece}, false) {
				return
			}
		}
	}

	chunks := splitIntoRuneChunks(contentText)
	for i, piece := range chunks {
		if !sendChunk(map[string]any{"content": piece}, i == len(chunks)-1) {
			return
		}
	}

	if len(chunks) == 0 && reasoningText == "" {
		sendChunk(map[string]any{"content": ""}, true)
	} else if len(chunks) == 0 {
		sendChunk(map[string]any{"content": ""}, true)
	}

	sw.write("data: [DONE]\n\n")
}

func firstChoiceField(oai map[string]any, field string) string {
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
	if c, ok := msg[field].(string); ok {
		return c
	}
	return ""
}

func firstChoiceContent(oai map[string]any) string {
	return firstChoiceField(oai, "content")
}

func firstChoiceReasoning(oai map[string]any) string {
	return firstChoiceField(oai, "reasoning_content")
}
