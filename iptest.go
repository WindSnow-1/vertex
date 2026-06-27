//go:build ignore

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"strings"

	"vertex/internal/config"
	"vertex/internal/proxy"
)

func main() {
	cfg := config.Load()
	uri := cfg.ActiveNodeURI
	if uri == "" {
		uri = cfg.ProxyURL
	}
	fmt.Println("configured active node:", uri)

	// If active node points to 127.0.0.1 (info placeholder), fall back to first real node.
	const realJP = "vless://fef4b2b8-cd61-4208-a955-e7536f698147@cfyes.lxy1015.top:443?type=ws&encryption=none&host=jp-lx.777076.xyz&path=%2Fliangxin%2Fjp1&headerType=none&quicSecurity=none&serviceName=&security=tls&fp=safari&insecure=0&sni=jp-lx.777076.xyz#real"
	if strings.Contains(uri, "127.0.0.1") {
		fmt.Println("NOTE: active node is 127.0.0.1 placeholder; testing real JP node instead.")
		uri = realJP
	}
	fmt.Println("testing node:", uri)

	transport, err := proxy.TransportForNode(uri)
	if err != nil {
		fmt.Println("proxy error:", err)
		return
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, ep := range []string{"https://api.ipify.org", "https://ipinfo.io/json"} {
		req, _ := http.NewRequestWithContext(ctx, "GET", ep, nil)
		resp, err := client.Do(req)
		if err != nil {
			fmt.Println(ep, "error:", err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Println(ep, "->", string(body))
	}
}
