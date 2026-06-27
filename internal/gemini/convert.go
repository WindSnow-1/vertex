package gemini

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

func convertToGemini(oaiReq map[string]any) (string, map[string]any) {
	model, _ := oaiReq["model"].(string)
	messages, _ := oaiReq["messages"].([]any)
	contents := make([]any, 0, len(messages))
	var systemParts []any
	toolIDToName := map[string]string{}

	for _, msgRaw := range messages {
		m, _ := msgRaw.(map[string]any)
		role, _ := m["role"].(string)
		content := m["content"]

		switch role {
		case "system", "developer":
			switch c := content.(type) {
			case string:
				systemParts = append(systemParts, map[string]any{"text": c})
			case []any:
				for _, item := range c {
					if im, ok := item.(map[string]any); ok {
						if t, _ := im["type"].(string); t == "text" || t == "" {
							systemParts = append(systemParts, map[string]any{"text": im["text"]})
						}
					} else if s, ok := item.(string); ok {
						systemParts = append(systemParts, map[string]any{"text": s})
					}
				}
			}

		case "user":
			parts := convertUserContent(content)
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "user", "parts": parts})
			}

		case "assistant":
			var parts []any
			if content != nil {
				if s, ok := content.(string); ok && s != "" {
					parts = append(parts, map[string]any{"text": s})
				}
			}
			if toolCalls, ok := m["tool_calls"].([]any); ok {
				for _, tc := range toolCalls {
					tcMap, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := tcMap["function"].(map[string]any)
					name, _ := fn["name"].(string)
					if name == "" {
						continue
					}
					tcID, _ := tcMap["id"].(string)
					if tcID != "" {
						toolIDToName[tcID] = name
					}
					args := parseArgs(fn["arguments"])
					parts = append(parts, map[string]any{
						"functionCall": map[string]any{"name": name, "args": args},
					})
				}
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "model", "parts": parts})
			}

		case "tool":
			tcID, _ := m["tool_call_id"].(string)
			name, _ := m["name"].(string)
			if name == "" {
				name = toolIDToName[tcID]
			}
			if name == "" {
				name = "unknown"
			}
			resp := coerceFunctionResponse(content)
			fr := map[string]any{"functionResponse": map[string]any{
				"name": name, "response": resp,
			}}
			if n := len(contents); n > 0 {
				if last, ok := contents[n-1].(map[string]any); ok && last["role"] == "function" {
					parts, _ := last["parts"].([]any)
					last["parts"] = append(parts, fr)
					continue
				}
			}
			contents = append(contents, map[string]any{"role": "function", "parts": []any{fr}})
		}
	}

	geminiPayload := map[string]any{"contents": contents}
	if len(systemParts) > 0 {
		geminiPayload["systemInstruction"] = map[string]any{"parts": systemParts}
	}

	// tools → functionDeclarations
	if tools, ok := oaiReq["tools"].([]any); ok && len(tools) > 0 {
		var funcDecls []any
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			if toStr(tm["type"]) != "function" {
				continue
			}
			fn, _ := tm["function"].(map[string]any)
			if fn == nil {
				continue
			}
			decl := map[string]any{"name": fn["name"]}
			if desc, ok := fn["description"]; ok {
				decl["description"] = desc
			}
			if params, ok := fn["parameters"].(map[string]any); ok && len(params) > 0 {
				decl["parameters"] = params
			} else {
				decl["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			funcDecls = append(funcDecls, decl)
		}
		if len(funcDecls) > 0 {
			geminiPayload["tools"] = []any{map[string]any{"functionDeclarations": funcDecls}}
		}
	}

	// tool_choice → toolConfig
	if tc, ok := oaiReq["tool_choice"]; ok {
		switch v := tc.(type) {
		case string:
			switch v {
			case "none":
				geminiPayload["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}
			case "auto":
				geminiPayload["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}
			case "required":
				geminiPayload["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}
			}
		case map[string]any:
			if v["type"] == "function" {
				if fn, ok := v["function"].(map[string]any); ok {
					if fnName, ok := fn["name"].(string); ok && fnName != "" {
						geminiPayload["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{
							"mode": "ANY", "allowedFunctionNames": []any{fnName},
						}}
					}
				}
			}
		}
	}

	// generationConfig
	genCfg := map[string]any{}
	for _, m := range []struct{ oai, gem string }{
		{"temperature", "temperature"},
		{"top_p", "topP"},
		{"top_k", "topK"},
		{"presence_penalty", "presencePenalty"},
		{"frequency_penalty", "frequencyPenalty"},
		{"seed", "seed"},
	} {
		if v, ok := oaiReq[m.oai]; ok && v != nil {
			genCfg[m.gem] = v
		}
	}
	if v, ok := oaiReq["max_tokens"]; ok && v != nil {
		genCfg["maxOutputTokens"] = v
	} else if v, ok := oaiReq["max_completion_tokens"]; ok && v != nil {
		genCfg["maxOutputTokens"] = v
	}
	if stop, ok := oaiReq["stop"]; ok && stop != nil {
		switch s := stop.(type) {
		case string:
			genCfg["stopSequences"] = []any{s}
		case []any:
			genCfg["stopSequences"] = s
		}
	}
	if rf, ok := oaiReq["response_format"].(map[string]any); ok {
		if t, _ := rf["type"].(string); t == "json_object" || t == "json_schema" {
			genCfg["responseMimeType"] = "application/json"
		}
	}

	// reasoning_effort → thinkingConfig
	if re, ok := oaiReq["reasoning_effort"].(string); ok {
		levelMap := map[string]string{"low": "LOW", "medium": "MEDIUM", "high": "HIGH"}
		if level, ok := levelMap[strings.ToLower(re)]; ok {
			genCfg["thinkingConfig"] = map[string]any{"thinkingLevel": level}
		}
	}

	// thinking
	if thinking, ok := oaiReq["thinking"].(map[string]any); ok {
		if tt, _ := thinking["type"].(string); tt == "enabled" || tt == "disabled" {
			tc := map[string]any{"thinkingLevel": "MEDIUM"}
			if tt == "disabled" {
				tc["thinkingLevel"] = "NONE"
			}
			if budget, ok := thinking["budget_tokens"]; ok && budget != nil {
				tc["thinkingBudget"] = budget
			}
			genCfg["thinkingConfig"] = tc
		}
	}

	if len(genCfg) > 0 {
		geminiPayload["generationConfig"] = genCfg
	}

	return model, geminiPayload
}

func convertUserContent(content any) []any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return []any{map[string]any{"text": s}}
	}
	list, ok := content.([]any)
	if !ok {
		return nil
	}
	var parts []any
	for _, itemRaw := range list {
		if s, ok := itemRaw.(string); ok {
			parts = append(parts, map[string]any{"text": s})
			continue
		}
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := item["type"].(string)
		switch t {
		case "text":
			parts = append(parts, map[string]any{"text": item["text"]})
		case "image_url":
			url := imageURLString(item["image_url"])
			if strings.HasPrefix(url, "data:") {
				if mime, b64 := parseDataURI(url); mime != "" && b64 != "" {
					parts = append(parts, map[string]any{"inlineData": map[string]any{
						"mimeType": mime, "data": b64,
					}})
				}
			} else if url != "" {
				parts = append(parts, map[string]any{"fileData": map[string]any{
					"mimeType": guessMIME(url), "fileUri": url,
				}})
			}
		}
	}
	return parts
}

func imageURLString(v any) string {
	if m, ok := v.(map[string]any); ok {
		s, _ := m["url"].(string)
		return s
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func parseDataURI(uri string) (string, string) {
	if !strings.HasPrefix(uri, "data:") {
		return "", ""
	}
	rest := uri[5:]
	idx := strings.Index(rest, ",")
	if idx < 0 {
		return "", ""
	}
	meta := rest[:idx]
	data := rest[idx+1:]
	mime := strings.TrimSuffix(meta, ";base64")
	return mime, data
}

func guessMIME(url string) string {
	lower := strings.ToLower(url)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func parseArgs(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if s, ok := v.(string); ok && s != "" {
		var parsed map[string]any
		if json.Unmarshal([]byte(s), &parsed) == nil {
			return parsed
		}
	}
	return map[string]any{}
}

func coerceFunctionResponse(raw any) map[string]any {
	if raw == nil {
		return map[string]any{}
	}
	if s, ok := raw.(string); ok {
		var parsed any
		if json.Unmarshal([]byte(s), &parsed) == nil {
			if m, ok := parsed.(map[string]any); ok {
				return m
			}
			return map[string]any{"result": parsed}
		}
		return map[string]any{"result": s}
	}
	if m, ok := raw.(map[string]any); ok {
		return m
	}
	return map[string]any{"result": raw}
}

// --- Gemini → OpenAI conversion ---

var finishReasonMap = map[string]string{
	"STOP":                    "stop",
	"MAX_TOKENS":              "length",
	"SAFETY":                  "content_filter",
	"RECITATION":              "content_filter",
	"PROHIBITED_CONTENT":      "content_filter",
	"TOOL_CALLS":              "tool_calls",
	"MALFORMED_FUNCTION_CALL": "tool_calls",
	"OTHER":                   "stop",
}

func mapFinishReason(finish string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	if finish == "" {
		return "stop"
	}
	if v, ok := finishReasonMap[strings.ToUpper(finish)]; ok {
		return v
	}
	return "stop"
}

func geminiToOAI(geminiResp map[string]any, model string) map[string]any {
	candidate := firstCandidate(geminiResp)
	parts := candidateParts(candidate)
	finish, _ := candidate["finishReason"].(string)

	text, toolCalls, reasoning := extractParts(parts)

	oaiFinish := mapFinishReason(finish, len(toolCalls) > 0)

	message := map[string]any{"role": "assistant"}
	if text != "" {
		message["content"] = text
	} else {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}

	result := map[string]any{
		"id":      "chatcmpl-" + generateID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": oaiFinish,
		}},
	}
	if usageMeta, ok := geminiResp["usageMetadata"].(map[string]any); ok {
		result["usage"] = convertUsage(usageMeta)
	}
	return result
}

func geminiChunkToSSE(chunk map[string]any, model, requestID string, isFirst bool) []string {
	candidate := firstCandidate(chunk)
	parts := candidateParts(candidate)
	finish, _ := candidate["finishReason"].(string)

	created := time.Now().Unix()
	base := func() map[string]any {
		return map[string]any{
			"id":      "chatcmpl-" + requestID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
		}
	}
	var events []string

	if isFirst {
		b := base()
		b["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}
		events = append(events, sseLine(b))
	}

	text, toolCalls, reasoning := extractParts(parts)

	if reasoning != "" {
		b := base()
		b["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": reasoning}, "finish_reason": nil}}
		events = append(events, sseLine(b))
	}
	if text != "" {
		b := base()
		b["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}}
		events = append(events, sseLine(b))
	}
	if len(toolCalls) > 0 {
		b := base()
		b["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": toolCalls}, "finish_reason": nil}}
		events = append(events, sseLine(b))
	}

	if finish != "" && finish != finishReasonUnspecified {
		oaiFinish := mapFinishReason(finish, len(toolCalls) > 0)
		finishEvt := base()
		choice := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": oaiFinish}
		finishEvt["choices"] = []any{choice}
		if usageMeta, ok := chunk["usageMetadata"].(map[string]any); ok && len(usageMeta) > 0 {
			finishEvt["usage"] = convertUsage(usageMeta)
		}
		events = append(events, sseLine(finishEvt))
	}

	return events
}

func extractParts(parts []any) (string, []any, string) {
	var texts []string
	var thoughts []string
	var toolCalls []any

	for _, pRaw := range parts {
		part, ok := pRaw.(map[string]any)
		if !ok {
			continue
		}
		textVal := toStr(part["text"])
		hasText := textVal != ""
		isThought := part["thought"] == true

		switch {
		case isFunctionCallWithName(part):
			fc, _ := part["functionCall"].(map[string]any)
			args := fc["args"]
			if args == nil {
				args = map[string]any{}
			}
			argBytes, _ := json.Marshal(args)
			tc := map[string]any{
				"index": len(toolCalls),
				"id":    "call_" + generateID(),
				"type":  "function",
				"function": map[string]any{
					"name":      toStr(fc["name"]),
					"arguments": string(argBytes),
				},
			}
			toolCalls = append(toolCalls, tc)
		case isThought && hasText:
			thoughts = append(thoughts, textVal)
		case hasText:
			texts = append(texts, textVal)
		}
	}

	textContent := strings.Join(texts, "")
	reasoning := strings.Join(thoughts, "")
	return textContent, toolCalls, reasoning
}

func isFunctionCallWithName(part map[string]any) bool {
	if fc, ok := part["functionCall"].(map[string]any); ok {
		name, _ := fc["name"].(string)
		return strings.TrimSpace(name) != ""
	}
	return false
}

func firstCandidate(resp map[string]any) map[string]any {
	if cands, ok := resp["candidates"].([]any); ok && len(cands) > 0 {
		if c, ok := cands[0].(map[string]any); ok {
			return c
		}
	}
	return map[string]any{}
}

func candidateParts(candidate map[string]any) []any {
	if content, ok := candidate["content"].(map[string]any); ok {
		if parts, ok := content["parts"].([]any); ok {
			return parts
		}
	}
	return nil
}

func convertUsage(meta map[string]any) map[string]any {
	prompt := numOf(meta["promptTokenCount"])
	completion := numOf(meta["candidatesTokenCount"]) + numOf(meta["thoughtsTokenCount"])
	total := prompt + completion
	if _, ok := meta["totalTokenCount"]; ok {
		total = numOf(meta["totalTokenCount"])
	}
	result := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	if t := numOf(meta["thoughtsTokenCount"]); t > 0 {
		result["completion_tokens_details"] = map[string]any{"reasoning_tokens": t}
	}
	return result
}

func numOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

func sseLine(obj map[string]any) string {
	data, err := json.Marshal(obj)
	if err != nil {
		return "data: {}\n\n"
	}
	return "data: " + string(data) + "\n\n"
}

func generateID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func formatOAIError(status int, message, errType string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    status,
		},
	}
}
