package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
)

const finishReasonUnspecified = "FINISH_REASON_UNSPECIFIED"

type streamChunk struct {
	Data map[string]any
	Err  *VertexError
}

func scanStream(body io.Reader, onObject func(map[string]any) (bool, error)) error {
	reader := bufio.NewReader(body)
	readBuf := make([]byte, 16*1024)

	var buffer []byte
	scanPos := 0
	startIdx := 0
	braceCount := 0
	inString := false
	escape := false

	const maxBufferSize = 512 * 1024

	for {
		n, readErr := reader.Read(readBuf)
		if n > 0 {
			buffer = append(buffer, readBuf[:n]...)

			if len(buffer) > maxBufferSize {
				buffer = buffer[scanPos:]
				scanPos = 0
				startIdx = 0
			}

			for {
				if scanPos == 0 {
					startIdx = bytes.IndexByte(buffer, '{')
					if startIdx == -1 {
						buffer = buffer[:0]
						break
					}
					scanPos = startIdx
					braceCount = 0
					inString = false
					escape = false
				}

				endIdx := -1
				for i := scanPos; i < len(buffer); i++ {
					ch := buffer[i]
					if escape {
						escape = false
						continue
					}
					if ch == '\\' {
						escape = true
						continue
					}
					if ch == '"' {
						inString = !inString
						continue
					}
					if !inString {
						if ch == '{' {
							braceCount++
						} else if ch == '}' {
							braceCount--
							if braceCount == 0 {
								endIdx = i
								break
							}
						}
					}
				}

				if endIdx != -1 {
					jsonStr := buffer[startIdx : endIdx+1]
					rest := make([]byte, len(buffer)-(endIdx+1))
					copy(rest, buffer[endIdx+1:])
					buffer = rest
					scanPos = 0

					var obj map[string]any
					if json.Unmarshal(jsonStr, &obj) == nil {
						stop, err := onObject(obj)
						if err != nil {
							return err
						}
						if stop {
							return nil
						}
					}
				} else {
					scanPos = len(buffer)
					break
				}
			}
		}

		if readErr != nil {
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				return readErr
			}
			return nil
		}
	}
}

func processStreamingObject(obj map[string]any, emit func(map[string]any) bool) (bool, error) {
	results, _ := obj["results"].([]any)
	for _, rRaw := range results {
		result, ok := rRaw.(map[string]any)
		if !ok {
			continue
		}

		if errs, ok := result["errors"].([]any); ok && len(errs) > 0 {
			errMsg := ""
			if first, ok := errs[0].(map[string]any); ok {
				errMsg = toStr(first["message"])
			} else {
				errMsg = toStr(errs[0])
			}
			if isAuthError(errMsg) {
				return false, newAuthError(errMsg)
			}
			if parsed := parseErrorResponse(map[string]any{"errors": errs}); parsed != nil {
				return false, parsed
			}
		}

		data, ok := result["data"].(map[string]any)
		if !ok {
			continue
		}

		if ui, ok := data["ui"].(map[string]any); ok {
			if innerRaw, exists := ui["streamGenerateContentAnonymous"]; exists {
				switch inner := innerRaw.(type) {
				case map[string]any:
					data = inner
				case []any:
					for _, itemRaw := range inner {
						if item, ok := itemRaw.(map[string]any); ok {
							if chunk := extractChunk(item); chunk != nil {
								if _, done := emitAndCheckFinish(chunk, emit); done {
									return true, nil
								}
							}
						}
					}
					continue
				default:
					continue
				}
			}
		}

		if chunk := extractChunk(data); chunk != nil {
			if _, done := emitAndCheckFinish(chunk, emit); done {
				return true, nil
			}
		}
	}
	return false, nil
}

func emitAndCheckFinish(chunk map[string]any, emit func(map[string]any) bool) (bool, bool) {
	if !emit(chunk) {
		log.Printf("[stream] client disconnected")
		return true, true
	}
	fr := chunkFinishReason(chunk)
	if fr != "" && fr != finishReasonUnspecified {
		return false, true
	}
	return false, false
}

func chunkFinishReason(chunk map[string]any) string {
	cands, ok := chunk["candidates"].([]any)
	if !ok || len(cands) == 0 {
		return ""
	}
	c, ok := cands[0].(map[string]any)
	if !ok {
		return ""
	}
	return toStr(c["finishReason"])
}

func extractChunk(data map[string]any) map[string]any {
	chunk := map[string]any{}

	if raw, ok := data["candidates"]; ok && raw != nil {
		candidatesRaw, _ := raw.([]any)
		if len(candidatesRaw) > 0 {
			cleaned := make([]any, 0, len(candidatesRaw))
			for _, cRaw := range candidatesRaw {
				candidate, ok := cRaw.(map[string]any)
				if !ok {
					continue
				}
				content, hasContent := candidate["content"].(map[string]any)
				if hasContent {
					parts, ok := content["parts"].([]any)
					if ok {
						cc := shallowCopy(candidate)
						role := toStr(content["role"])
						if role == "" {
							role = "model"
						}
						cc["content"] = map[string]any{"role": role, "parts": parts}
						cleaned = append(cleaned, cc)
					} else {
						cleaned = append(cleaned, candidate)
					}
				} else {
					cleaned = append(cleaned, candidate)
				}
			}
			if len(cleaned) > 0 {
				chunk["candidates"] = cleaned
			} else {
				chunk["candidates"] = candidatesRaw
			}
		} else {
			chunk["candidates"] = candidatesRaw
		}
	}

	for _, key := range []string{"usageMetadata", "modelVersion", "responseId", "promptFeedback"} {
		if v, ok := data[key]; ok && v != nil {
			switch x := v.(type) {
			case string:
				if x != "" {
					chunk[key] = v
				}
			case []any:
				if len(x) > 0 {
					chunk[key] = v
				}
			case map[string]any:
				if len(x) > 0 {
					chunk[key] = v
				}
			default:
				chunk[key] = v
			}
		}
	}

	if len(chunk) == 0 {
		return nil
	}
	return chunk
}

func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func isTruthyStr(v string) bool {
	return strings.TrimSpace(v) != ""
}
