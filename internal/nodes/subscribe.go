package nodes

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

func FetchSubscription(subURL string) ([]Node, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", subURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return ParseSubscriptionText(string(data)), nil
}

func ParseSubscriptionText(text string) []Node {
	text = strings.TrimSpace(text)
	var lines []string

	if strings.Contains(text, "proxies:") {
		lines = parseClashYaml(text)
	} else {
		if decoded, err := decodeBase64Sub(text); err == nil {
			text = decoded
		}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				lines = append(lines, line)
			}
		}
	}

	var result []Node
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out, err := ParseURI(line)
		if err != nil {
			continue
		}
		t, _ := out["type"].(string)
		name := ExtractNodeName(line)
		if n, ok := out["name"].(string); ok && n != "" {
			name = n
		}
		result = append(result, Node{Type: t, Name: name, RawURI: line})
	}
	return result
}

func decodeBase64Sub(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, " ", "")
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return string(b), nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return string(b), nil
	}
	t := strings.ReplaceAll(strings.ReplaceAll(s, "-", "+"), "_", "/")
	if pad := len(t) % 4; pad != 0 {
		t += strings.Repeat("=", 4-pad)
	}
	b, err := base64.StdEncoding.DecodeString(t)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func parseClashYaml(text string) []string {
	var uris []string
	lines := strings.Split(text, "\n")
	inProxies := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "proxies:") {
			inProxies = true
			continue
		}
		if inProxies && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && strings.Contains(trimmed, ":") {
			inProxies = false
		}
		if !inProxies {
			continue
		}
		if strings.HasPrefix(trimmed, "- {") && strings.HasSuffix(trimmed, "}") {
			attrs := parseInlineYamlAttrs(trimmed[3 : len(trimmed)-1])
			if uri := clashProxyToURI(attrs); uri != "" {
				uris = append(uris, uri)
			}
		}
	}
	return uris
}

func parseInlineYamlAttrs(s string) map[string]string {
	attrs := make(map[string]string)
	var currentKey, currentValue strings.Builder
	inQuotes := false
	var quoteChar rune
	isKey := true
	braceDepth := 0
	bracketDepth := 0

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if inQuotes {
			if r == quoteChar {
				inQuotes = false
			} else if r == '\\' && i+1 < len(runes) {
				if isKey {
					currentKey.WriteRune(runes[i+1])
				} else {
					currentValue.WriteRune(runes[i+1])
				}
				i++
			} else {
				if isKey {
					currentKey.WriteRune(r)
				} else {
					currentValue.WriteRune(r)
				}
			}
			continue
		}
		if r == '"' || r == '\'' {
			inQuotes = true
			quoteChar = r
			continue
		}
		if isKey {
			if r == ':' {
				isKey = false
				if i+1 < len(runes) && runes[i+1] == ' ' {
					i++
				}
			} else if r != ' ' && r != '\t' {
				currentKey.WriteRune(r)
			}
		} else {
			switch r {
			case '{':
				braceDepth++
				currentValue.WriteRune(r)
			case '}':
				if braceDepth > 0 {
					braceDepth--
				}
				currentValue.WriteRune(r)
			case '[':
				bracketDepth++
				currentValue.WriteRune(r)
			case ']':
				if bracketDepth > 0 {
					bracketDepth--
				}
				currentValue.WriteRune(r)
			case ',':
				if braceDepth > 0 || bracketDepth > 0 {
					currentValue.WriteRune(r)
					continue
				}
				key := strings.TrimSpace(currentKey.String())
				val := strings.TrimSpace(currentValue.String())
				if key != "" {
					attrs[key] = val
				}
				currentKey.Reset()
				currentValue.Reset()
				isKey = true
			default:
				currentValue.WriteRune(r)
			}
		}
	}
	key := strings.TrimSpace(currentKey.String())
	val := strings.TrimSpace(currentValue.String())
	if key != "" {
		attrs[key] = val
	}
	return attrs
}

func clashProxyToURI(attrs map[string]string) string {
	typ := strings.ToLower(strings.TrimSpace(attrs["type"]))
	name := attrs["name"]
	server := attrs["server"]
	port := attrs["port"]
	if server == "" || port == "" {
		return ""
	}

	switch typ {
	case "ss":
		cipher := attrs["cipher"]
		password := attrs["password"]
		if cipher == "" || password == "" {
			return ""
		}
		userinfo := base64.StdEncoding.EncodeToString([]byte(cipher + ":" + password))
		return "ss://" + userinfo + "@" + server + ":" + port + "#" + strings.ReplaceAll(name, " ", "%20")

	case "vmess":
		uuid := attrs["uuid"]
		aidStr := attrs["alterId"]
		if aidStr == "" {
			aidStr = "0"
		}
		vmessJSON := map[string]any{
			"v": "2", "ps": name, "add": server, "port": port,
			"id": uuid, "aid": aidStr, "net": "tcp", "type": "none",
			"host": "", "path": "", "tls": "",
		}
		if attrs["network"] == "ws" {
			vmessJSON["net"] = "ws"
		}
		if attrs["tls"] == "true" {
			vmessJSON["tls"] = "tls"
		}
		b, _ := json.Marshal(vmessJSON)
		return "vmess://" + base64.StdEncoding.EncodeToString(b)

	case "vless":
		uuid := attrs["uuid"]
		if uuid == "" {
			return ""
		}
		u := "vless://" + uuid + "@" + server + ":" + port
		params := []string{}
		if attrs["tls"] == "true" {
			params = append(params, "security=tls")
		}
		if sni := attrs["sni"]; sni != "" {
			params = append(params, "sni="+sni)
		} else if sn := attrs["servername"]; sn != "" {
			params = append(params, "sni="+sn)
		}
		if flow := attrs["flow"]; flow != "" {
			params = append(params, "flow="+flow)
		}
		if network := attrs["network"]; network != "" {
			params = append(params, "type="+network)
		}
		if len(params) > 0 {
			u += "?" + strings.Join(params, "&")
		}
		u += "#" + strings.ReplaceAll(name, " ", "%20")
		return u

	case "trojan":
		password := attrs["password"]
		if password == "" {
			return ""
		}
		u := "trojan://" + password + "@" + server + ":" + port
		params := []string{}
		if sni := attrs["sni"]; sni != "" {
			params = append(params, "sni="+sni)
		} else if sn := attrs["servername"]; sn != "" {
			params = append(params, "sni="+sn)
		}
		if network := attrs["network"]; network != "" {
			params = append(params, "type="+network)
		}
		if len(params) > 0 {
			u += "?" + strings.Join(params, "&")
		}
		u += "#" + strings.ReplaceAll(name, " ", "%20")
		return u

	case "hysteria2", "hy2":
		password := attrs["password"]
		if password == "" {
			return ""
		}
		u := "hy2://" + password + "@" + server + ":" + port
		params := []string{}
		if sni := attrs["sni"]; sni != "" {
			params = append(params, "sni="+sni)
		}
		if len(params) > 0 {
			u += "?" + strings.Join(params, "&")
		}
		u += "#" + strings.ReplaceAll(name, " ", "%20")
		return u
	}

	return ""
}
