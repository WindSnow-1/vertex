package gemini

import (
	"encoding/json"
	"strconv"
	"strings"
)

const (
	StatusInvalidArgument   = "INVALID_ARGUMENT"
	StatusNotFound          = "NOT_FOUND"
	StatusPermissionDenied  = "PERMISSION_DENIED"
	StatusResourceExhausted = "RESOURCE_EXHAUSTED"
	StatusInternal          = "INTERNAL"
	StatusUnavailable       = "UNAVAILABLE"
	StatusUnauthenticated   = "UNAUTHENTICATED"
)

type VertexError struct {
	Message    string
	Code       int
	Status     string
	Kind       string
	RetryAfter int
}

func (e *VertexError) Error() string { return e.Message }

func (e *VertexError) IsRetryable() bool {
	switch e.Code {
	case 408, 429, 500, 502, 503, 504:
		return true
	}
	return e.Kind == "auth"
}

func newAuthError(msg string) *VertexError {
	return &VertexError{Message: msg, Code: 502, Status: StatusUnauthenticated, Kind: "auth"}
}

func newRateLimitError(msg string, retryAfter int) *VertexError {
	return &VertexError{Message: msg, Code: 429, Status: StatusResourceExhausted, Kind: "ratelimit", RetryAfter: retryAfter}
}

func newInvalidArgError(msg string) *VertexError {
	return &VertexError{Message: msg, Code: 400, Status: StatusInvalidArgument, Kind: "invalid"}
}

func newNotFoundError(msg string) *VertexError {
	return &VertexError{Message: msg, Code: 404, Status: StatusNotFound, Kind: "notfound"}
}

func newInternalError(msg string) *VertexError {
	return &VertexError{Message: msg, Code: 500, Status: StatusInternal, Kind: "internal"}
}

func newEmptyResponseError(msg string) *VertexError {
	return &VertexError{Message: msg, Code: 502, Status: StatusInternal, Kind: "empty"}
}

func raiseForStatus(code int, status, message string) *VertexError {
	switch {
	case status == StatusResourceExhausted || code == 8 || code == 429:
		return newRateLimitError(message, 0)
	case status == StatusUnauthenticated || code == 16 || code == 401:
		return newAuthError(message)
	case status == StatusPermissionDenied || code == 7 || code == 403:
		return &VertexError{Message: message, Code: 403, Status: StatusPermissionDenied, Kind: "permission"}
	case status == StatusInvalidArgument || code == 3 || code == 400:
		return newInvalidArgError(message)
	case status == StatusNotFound || code == 5 || code == 404:
		return newNotFoundError(message)
	case status == StatusUnavailable || code == 14 || code == 503:
		return &VertexError{Message: message, Code: 503, Status: StatusUnavailable, Kind: "unavailable"}
	case code >= 400 && code < 600:
		return &VertexError{Message: message, Code: code, Status: status, Kind: "client"}
	default:
		c := code
		if c == 0 || c < 100 {
			c = 500
		}
		return &VertexError{Message: message, Code: c, Status: status, Kind: "server"}
	}
}

func parseErrorResponse(data any) *VertexError {
	switch v := data.(type) {
	case string:
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			return nil
		}
		return parseErrorResponse(parsed)
	case []any:
		for _, item := range v {
			if e := parseErrorResponse(item); e != nil {
				return e
			}
		}
		return nil
	case map[string]any:
		if errObj, ok := v["error"].(map[string]any); ok {
			return raiseForStatus(
				toInt(errObj["code"], 500), toStr(errObj["status"]),
				toStrOr(errObj["message"], "Unknown error"),
			)
		}
		if errs, ok := v["errors"].([]any); ok && len(errs) > 0 {
			if first, ok := errs[0].(map[string]any); ok {
				ext := toMap(first["extensions"])
				extStatus := toMap(ext["status"])
				code := toInt(firstNonNil(extStatus["code"], first["code"]), 500)
				status := toStr(firstNonNil(extStatus["status"], first["status"]))
				message := toStrOr(firstNonNil(extStatus["message"], first["message"]), "Unknown error")
				return raiseForStatus(code, status, message)
			}
		}
		if _, hasCode := v["code"]; hasCode {
			return raiseForStatus(toInt(v["code"], 500), toStr(v["status"]), toStrOr(v["message"], "Unknown error"))
		}
		if _, hasMsg := v["message"]; hasMsg {
			return raiseForStatus(toInt(v["code"], 500), toStr(v["status"]), toStrOr(v["message"], "Unknown error"))
		}
		return nil
	default:
		return nil
	}
}

func asVertexError(err error) *VertexError {
	if ve, ok := err.(*VertexError); ok {
		return ve
	}
	return nil
}

func toInt(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if x, err := strconv.Atoi(n); err == nil {
			return x
		}
	}
	return def
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toStrOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func toMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func marshalStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func isAuthError(msg string) bool {
	return strings.Contains(msg, "Failed to verify action") ||
		strings.Contains(msg, "The caller does not have permission")
}
