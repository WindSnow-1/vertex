package gemini

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type parseResult struct {
	Parts         []map[string]any
	FinishReason  string
	UsageMetadata map[string]any
	PromptFeedback map[string]any
	ModelVersion  any
	ResponseID    any
	HasError      bool
	ErrorMessage  string
	ErrorObj      *VertexError
}

var trailingCommaBeforeBracket = regexp.MustCompile(`,\s*\]`)

func parseUpstreamData(raw string) *parseResult {
	s := &parseResult{
		PromptFeedback: map[string]any{},
		UsageMetadata:  map[string]any{},
	}
	partsByPath := map[int]map[string]any{}
	var unindexed []map[string]any

	cleaned := cleanJSONString(raw)
	var dataList []any
	if err := json.Unmarshal([]byte(cleaned), &dataList); err != nil {
		var single any
		if err2 := json.Unmarshal([]byte(cleaned), &single); err2 == nil {
			dataList = []any{single}
		} else {
			s.HasError = true
			s.ErrorMessage = "JSON parse error: " + err.Error()
			s.Parts = []map[string]any{}
			return s
		}
	}

	for _, itemRaw := range dataList {
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}

		if parsed := parseErrorResponse(item); parsed != nil {
			if strings.Contains(parsed.Message, "Failed to verify action") {
				// ignore - expected first-frame auth error
			} else if !s.HasError {
				s.HasError = true
				s.ErrorMessage = parsed.Message
				s.ErrorObj = parsed
			}
		}

		if msg := extractErrorMessage(item); msg != "" && !s.HasError {
			s.HasError = true
			s.ErrorMessage = msg
		}

		results, ok := item["results"].([]any)
		if !ok {
			continue
		}

		for _, rRaw := range results {
			result, ok := rRaw.(map[string]any)
			if !ok {
				continue
			}

			if errs, ok := result["errors"].([]any); ok && len(errs) > 0 {
				if parsed := parseErrorResponse(map[string]any{"errors": errs}); parsed != nil {
					s.HasError = true
					s.ErrorMessage = parsed.Message
					s.ErrorObj = parsed
				}
			}

			if result["data"] == nil {
				continue
			}

			pathIndex := extractPathIndex(result)
			if data, ok := result["data"].(map[string]any); ok {
				updateStateFromData(s, partsByPath, &unindexed, data, pathIndex)
			}
		}
	}

	keys := make([]int, 0, len(partsByPath))
	for k := range partsByPath {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	ordered := make([]map[string]any, 0, len(keys)+len(unindexed))
	for _, k := range keys {
		ordered = append(ordered, partsByPath[k])
	}
	ordered = append(ordered, unindexed...)

	s.Parts = mergeContentBlocks(ordered)
	return s
}

func cleanJSONString(raw string) string {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return "[]"
	}
	cleaned = strings.TrimSuffix(cleaned, ",")
	cleaned = trailingCommaBeforeBracket.ReplaceAllString(cleaned, "]")
	if !strings.HasPrefix(cleaned, "[") {
		cleaned = "[" + cleaned + "]"
	} else if !strings.HasSuffix(cleaned, "]") {
		cleaned += "]"
	}
	return cleaned
}

func extractPathIndex(result map[string]any) int {
	path, ok := result["path"].([]any)
	if !ok || len(path) == 0 {
		return -1
	}
	for i := len(path) - 1; i >= 0; i-- {
		switch v := path[i].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return -1
}

func updateStateFromData(s *parseResult, partsByPath map[int]map[string]any, unindexed *[]map[string]any, data map[string]any, pathIndex int) {
	if ui, ok := data["ui"].(map[string]any); ok {
		if inner, ok := ui["streamGenerateContentAnonymous"].(map[string]any); ok {
			data = inner
		}
	}

	if pf, ok := data["promptFeedback"].(map[string]any); ok && len(pf) > 0 {
		s.PromptFeedback = pf
	}
	if um, ok := data["usageMetadata"].(map[string]any); ok && len(um) > 0 {
		s.UsageMetadata = um
	}
	if v, ok := data["modelVersion"]; ok {
		s.ModelVersion = v
	}
	if v, ok := data["responseId"]; ok {
		s.ResponseID = v
	}

	candidates, _ := data["candidates"].([]any)
	for _, cRaw := range candidates {
		c, ok := cRaw.(map[string]any)
		if !ok {
			continue
		}
		if fr, ok := c["finishReason"].(string); ok && fr != "" {
			s.FinishReason = fr
		}
		content, _ := c["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, pRaw := range parts {
			if p, ok := pRaw.(map[string]any); ok {
				if pathIndex != -1 {
					partsByPath[pathIndex] = p
				} else {
					*unindexed = append(*unindexed, p)
				}
			}
		}
	}
}

func extractErrorMessage(item map[string]any) string {
	if errObj, ok := item["error"]; ok && errObj != nil {
		if m, ok := errObj.(map[string]any); ok {
			return toStrOr(m["message"], marshalStr(m))
		}
		return toStr(errObj)
	}
	if errs, ok := item["errors"].([]any); ok && len(errs) > 0 {
		if m, ok := errs[0].(map[string]any); ok {
			return toStrOr(m["message"], marshalStr(m))
		}
	}
	return ""
}

func mergeContentBlocks(parts []map[string]any) []map[string]any {
	if len(parts) <= 1 {
		return parts
	}
	merged := []map[string]any{parts[0]}
	for _, p := range parts[1:] {
		last := merged[len(merged)-1]
		lastText, lastIsText := last["text"].(string)
		curText, curIsText := p["text"].(string)
		if lastIsText && curIsText {
			last["text"] = lastText + curText
		} else {
			merged = append(merged, p)
		}
	}
	return merged
}
