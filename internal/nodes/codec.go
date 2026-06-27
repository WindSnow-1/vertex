package nodes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func padB64(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "-", "+"), "_", "/")
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	return s
}

func ParseURI(uri string) (map[string]any, error) {
	switch {
	case strings.HasPrefix(uri, "vless://"):
		return parseSimple(uri, "vless")
	case strings.HasPrefix(uri, "trojan://"):
		return parseSimple(uri, "trojan")
	case strings.HasPrefix(uri, "vmess://"):
		return parseVmess(uri)
	case strings.HasPrefix(uri, "ss://"):
		return parseShadowsocks(uri)
	case strings.HasPrefix(uri, "hysteria2://"), strings.HasPrefix(uri, "hy2://"):
		return parseSimple(uri, "hysteria2")
	case strings.HasPrefix(uri, "tuic://"):
		return parseSimple(uri, "tuic")
	}
	return nil, fmt.Errorf("unsupported protocol")
}

func parseSimple(uri, typ string) (map[string]any, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	q := u.Query()
	name := u.Fragment
	if dec, err := url.QueryUnescape(name); err == nil {
		name = dec
	}
	out := map[string]any{"name": name, "type": typ, "server": u.Hostname(), "port": port}
	if typ == "trojan" || typ == "hysteria2" {
		out["password"] = u.User.Username()
	} else {
		out["uuid"] = u.User.Username()
	}

	sec := strings.ToLower(q.Get("security"))
	if sec == "tls" || sec == "reality" || typ == "trojan" || typ == "hysteria2" || typ == "tuic" {
		out["tls"] = true
		sni := q.Get("sni")
		if sni == "" {
			sni = u.Hostname()
		}
		out["sni"] = sni
		out["servername"] = sni
		out["skip-cert-verify"] = true
	}

	if sec == "reality" {
		pubKey := q.Get("pbk")
		if pubKey == "" {
			pubKey = q.Get("public-key")
		}
		if pubKey != "" {
			sid := q.Get("sid")
			if sid == "" {
				sid = q.Get("short-id")
			}
			out["reality-opts"] = map[string]any{"public-key": pubKey, "short-id": sid}
		}
	}

	if typ == "vless" || typ == "trojan" {
		if flow := q.Get("flow"); flow != "" {
			out["flow"] = flow
		}
		fp := q.Get("fp")
		if fp == "" {
			fp = q.Get("client-fingerprint")
		}
		if fp != "" {
			out["client-fingerprint"] = fp
		}
		network := q.Get("type")
		if network == "ws" || network == "grpc" || network == "http" || network == "xhttp" {
			out["network"] = network
			switch network {
			case "ws":
				path := q.Get("path")
				if path == "" {
					path = "/"
				}
				host := q.Get("host")
				wsOpts := map[string]any{"path": path}
				if host != "" {
					wsOpts["headers"] = map[string]any{"Host": host}
				}
				out["ws-opts"] = wsOpts
			case "grpc":
				if sn := q.Get("serviceName"); sn != "" {
					out["grpc-opts"] = map[string]any{"grpc-service-name": sn}
				}
			}
		}
		if alpn := q.Get("alpn"); alpn != "" {
			out["alpn"] = strings.Split(alpn, ",")
		}
	}

	if typ == "hysteria2" {
		sni := q.Get("sni")
		if sni == "" {
			sni = q.Get("peer")
		}
		if sni == "" {
			sni = u.Hostname()
		}
		out["sni"] = sni
		out["servername"] = sni
		if ports := q.Get("ports"); ports != "" {
			out["ports"] = ports
		}
		if obfs := q.Get("obfs"); obfs != "" {
			out["obfs"] = obfs
		}
		if obfsPw := q.Get("obfs-password"); obfsPw != "" {
			out["obfs-password"] = obfsPw
		}
		if fp := q.Get("fp"); fp != "" {
			out["fingerprint"] = fp
		}
		if alpn := q.Get("alpn"); alpn != "" {
			out["alpn"] = strings.Split(alpn, ",")
		}
	}

	return out, nil
}

func parseVmess(uri string) (map[string]any, error) {
	b64Str := uri[8:]
	if idx := strings.Index(b64Str, "?"); idx != -1 {
		b64Str = b64Str[:idx]
	}
	if idx := strings.Index(b64Str, "#"); idx != -1 {
		b64Str = b64Str[:idx]
	}
	b, err := base64.StdEncoding.DecodeString(padB64(b64Str))
	if err != nil {
		return nil, err
	}
	var d map[string]any
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	portStr := fmt.Sprintf("%v", d["port"])
	port, _ := strconv.Atoi(portStr)

	out := map[string]any{
		"name":   d["ps"],
		"type":   "vmess",
		"server": d["add"],
		"port":   port,
		"uuid":   d["id"],
		"cipher": "auto",
	}

	if aidVal, ok := d["aid"]; ok {
		switch v := aidVal.(type) {
		case float64:
			out["alterId"] = int(v)
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				out["alterId"] = n
			}
		}
	}

	tlsStr, _ := d["tls"].(string)
	if strings.ToLower(tlsStr) == "tls" {
		host, _ := d["host"].(string)
		sni := host
		if sni == "" {
			sni, _ = d["add"].(string)
		}
		out["tls"] = true
		out["sni"] = sni
		out["servername"] = sni
		out["skip-cert-verify"] = true
	}

	netType, _ := d["net"].(string)
	netType = strings.ToLower(strings.TrimSpace(netType))
	if netType != "" && netType != "tcp" {
		path, _ := d["path"].(string)
		host, _ := d["host"].(string)
		out["network"] = netType
		switch netType {
		case "ws":
			out["ws-opts"] = map[string]any{
				"path":    path,
				"headers": map[string]any{"Host": host},
			}
		case "grpc":
			out["grpc-opts"] = map[string]any{"grpc-service-name": path}
		case "http", "h2":
			hPath := path
			if hPath == "" {
				hPath = "/"
			}
			out["http-opts"] = map[string]any{
				"method":  "GET",
				"path":    []string{hPath},
				"headers": map[string][]string{"Host": {host}},
			}
		}
	}

	return out, nil
}

func parseShadowsocks(uri string) (map[string]any, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	name := u.Fragment
	if dec, err := url.QueryUnescape(name); err == nil {
		name = dec
	}

	if u.User != nil && u.Hostname() != "" {
		method, password, err := decodeSSUserInfo(u.User)
		if err != nil {
			return nil, err
		}
		port, _ := strconv.Atoi(u.Port())
		if port == 0 {
			return nil, fmt.Errorf("ss: invalid port")
		}
		out := map[string]any{
			"name": name, "type": "ss", "server": u.Hostname(),
			"port": port, "cipher": method, "password": password,
		}
		if plugin := u.Query().Get("plugin"); plugin != "" {
			applySSPlugin(out, plugin)
		}
		return out, nil
	}

	body := uri[5:]
	if idx := strings.Index(body, "#"); idx != -1 {
		body = body[:idx]
	}
	if idx := strings.Index(body, "@"); idx != -1 {
		userInfo := body[:idx]
		hp := body[idx+1:]
		parts := strings.SplitN(hp, ":", 2)
		if len(parts) < 2 {
			return nil, fmt.Errorf("ss: invalid host:port")
		}
		portStr := parts[1]
		if fragIdx := strings.Index(portStr, "#"); fragIdx != -1 {
			portStr = portStr[:fragIdx]
		}
		port, _ := strconv.Atoi(portStr)

		b, err := base64.StdEncoding.DecodeString(padB64(userInfo))
		if err != nil {
			return nil, fmt.Errorf("ss: decode userinfo failed")
		}
		methodPw := strings.SplitN(string(b), ":", 2)
		if len(methodPw) != 2 {
			return nil, fmt.Errorf("ss: invalid userinfo")
		}
		return map[string]any{
			"name": name, "type": "ss", "server": parts[0],
			"port": port, "cipher": methodPw[0], "password": methodPw[1],
		}, nil
	}

	return nil, fmt.Errorf("ss: parse failed")
}

func decodeSSUserInfo(user *url.Userinfo) (string, string, error) {
	if password, ok := user.Password(); ok {
		return user.Username(), password, nil
	}
	username := user.Username()
	if idx := strings.Index(username, ":"); idx != -1 {
		return username[:idx], username[idx+1:], nil
	}
	b, err := base64.StdEncoding.DecodeString(padB64(username))
	if err != nil {
		return "", "", fmt.Errorf("ss: decode failed")
	}
	parts := strings.SplitN(string(b), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("ss: invalid format")
	}
	return parts[0], parts[1], nil
}

func applySSPlugin(out map[string]any, pluginRaw string) {
	segments := strings.Split(pluginRaw, ";")
	plugin := strings.ToLower(strings.TrimSpace(segments[0]))
	rawOpts := map[string]string{}
	for _, seg := range segments[1:] {
		key, value, ok := strings.Cut(seg, "=")
		if !ok {
			continue
		}
		rawOpts[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
	}
	switch plugin {
	case "simple-obfs", "obfs-local", "obfs":
		out["plugin"] = "obfs"
		opts := map[string]any{}
		if mode := rawOpts["obfs"]; mode != "" {
			opts["mode"] = mode
		} else if mode := rawOpts["mode"]; mode != "" {
			opts["mode"] = mode
		}
		if host := rawOpts["obfs-host"]; host != "" {
			opts["host"] = host
		} else if host := rawOpts["host"]; host != "" {
			opts["host"] = host
		}
		if len(opts) > 0 {
			out["plugin-opts"] = opts
		}
	default:
		out["plugin"] = plugin
		if len(rawOpts) > 0 {
			opts := make(map[string]any, len(rawOpts))
			for k, v := range rawOpts {
				opts[k] = v
			}
			out["plugin-opts"] = opts
		}
	}
}

func ExtractNodeName(line string) string {
	if strings.HasPrefix(line, "vmess://") {
		b64Str := line[8:]
		if idx := strings.Index(b64Str, "?"); idx != -1 {
			b64Str = b64Str[:idx]
		}
		if idx := strings.Index(b64Str, "#"); idx != -1 {
			b64Str = b64Str[:idx]
		}
		if b, err := base64.StdEncoding.DecodeString(padB64(b64Str)); err == nil {
			var d map[string]any
			if json.Unmarshal(b, &d) == nil {
				if ps, ok := d["ps"].(string); ok && ps != "" {
					return ps
				}
			}
		}
	}
	if idx := strings.Index(line, "#"); idx != -1 {
		name := line[idx+1:]
		if dec, err := url.QueryUnescape(name); err == nil {
			return dec
		}
		return name
	}
	if len(line) > 40 {
		return line[:40]
	}
	return line
}
