package proxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"vertex/internal/nodes"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/constant"
)

type proxyEntry struct {
	proxy      constant.Proxy
	lastUsedAt time.Time
}

var (
	mu       sync.RWMutex
	proxyMap = make(map[string]*proxyEntry)
)

func GetDialer(uri string) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	if uri == "" {
		return nil, nil
	}
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") || strings.HasPrefix(uri, "socks5://") {
		return makeStdProxyDialer(uri), nil
	}
	return getMihomoDialer(uri)
}

func TransportForNode(uri string) (*http.Transport, error) {
	if uri == "" {
		return &http.Transport{}, nil
	}
	if strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://") || strings.HasPrefix(uri, "socks5://") {
		proxyURL, err := url.Parse(uri)
		if err != nil {
			return nil, err
		}
		return &http.Transport{Proxy: http.ProxyURL(proxyURL)}, nil
	}
	dialer, err := getMihomoDialer(uri)
	if err != nil {
		return nil, err
	}
	return &http.Transport{DialContext: dialer}, nil
}

func getMihomoDialer(uri string) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	mu.RLock()
	if entry, ok := proxyMap[uri]; ok {
		entry.lastUsedAt = time.Now()
		p := entry.proxy
		mu.RUnlock()
		return makeMihomoDialer(p), nil
	}
	mu.RUnlock()

	outMap, err := nodes.ParseURI(uri)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URI: %w", err)
	}

	p, err := adapter.ParseProxy(outMap)
	if err != nil {
		return nil, fmt.Errorf("create proxy adapter: %w", err)
	}

	mu.Lock()
	proxyMap[uri] = &proxyEntry{proxy: p, lastUsedAt: time.Now()}
	mu.Unlock()

	return makeMihomoDialer(p), nil
}

func makeMihomoDialer(p constant.Proxy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		port, _ := strconv.Atoi(portStr)
		metadata := &constant.Metadata{
			NetWork: constant.TCP,
			Type:    constant.HTTP,
			Host:    host,
			DstPort: uint16(port),
		}
		conn, err := p.DialContext(ctx, metadata)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}
}

func makeStdProxyDialer(proxyURL string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, err
		}
		var d net.Dialer
		proxyAddr := u.Host
		conn, err := d.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, err
		}
		if u.Scheme == "socks5" {
			return conn, nil
		}
		connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
		_, err = conn.Write([]byte(connectReq))
		if err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

func RemoveProxy(uri string) {
	mu.Lock()
	delete(proxyMap, uri)
	mu.Unlock()
}

func StartGC(interval, maxIdle time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			mu.Lock()
			now := time.Now()
			for uri, entry := range proxyMap {
				if now.Sub(entry.lastUsedAt) > maxIdle {
					delete(proxyMap, uri)
					log.Printf("[proxy] gc idle: %s", nodes.GetNodeName(uri))
				}
			}
			mu.Unlock()
		}
	}()
}

func StopAll() {
	mu.Lock()
	proxyMap = make(map[string]*proxyEntry)
	mu.Unlock()
}
