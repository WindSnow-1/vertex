package gemini

import "vertex/internal/config"

const (
	querySignature = "2/l8eCsMMY49imcDQ/lwwXyL8cYtTjxZBF2dNqy69LodY="
	operationName  = "StreamGenerateContentAnonymous"
)

func buildRequestPayload(model string, geminiPayload map[string]any, recaptchaToken string, cfg *config.AppConfig) map[string]any {
	vars := buildVariables(model, geminiPayload, cfg)
	vars["region"] = "global"
	vars["recaptchaToken"] = recaptchaToken
	return map[string]any{
		"requestContext": map[string]any{
			"clientVersion": "boq_cloud-boq-clientweb-vertexaistudio_20260402.09_p0",
			"pagePath":      "/vertex-ai/studio/multimodal",
			"jurisdiction":  "global",
			"localizationData": map[string]any{
				"locale":   "zh_CN",
				"timezone": "Asia/Shanghai",
			},
		},
		"querySignature": querySignature,
		"operationName":  operationName,
		"variables":      vars,
	}
}

var safetyCategories = []string{
	"HARM_CATEGORY_HARASSMENT",
	"HARM_CATEGORY_HATE_SPEECH",
	"HARM_CATEGORY_SEXUALLY_EXPLICIT",
	"HARM_CATEGORY_DANGEROUS_CONTENT",
	"HARM_CATEGORY_CIVIC_INTEGRITY",
}

var supportedVarFields = []string{
	"contents", "tools", "toolConfig", "systemInstruction", "safetySettings", "generationConfig",
}

func buildVariables(model string, geminiPayload map[string]any, cfg *config.AppConfig) map[string]any {
	vars := map[string]any{}
	vars["model"] = config.ResolveModel(model)

	for _, field := range supportedVarFields {
		if v, ok := geminiPayload[field]; ok {
			vars[field] = v
		}
	}

	if _, ok := vars["safetySettings"]; !ok {
		settings := make([]any, 0, len(safetyCategories))
		for _, cat := range safetyCategories {
			settings = append(settings, map[string]any{"category": cat, "threshold": "BLOCK_NONE"})
		}
		vars["safetySettings"] = settings
	}

	return vars
}
