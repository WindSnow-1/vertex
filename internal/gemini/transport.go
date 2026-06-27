package gemini

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"vertex/internal/proxy"
)

const (
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	chUA      = `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`
)

type session struct {
	client   tls_client.HttpClient
	proxyURI string
}

func (s *session) do(ctx context.Context, method, url string, header http.Header, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)
	if header != nil {
		req.Header = header
	}
	return s.client.Do(req)
}

func (s *session) doAndRead(ctx context.Context, method, url string, header http.Header, body io.Reader) (int, []byte, error) {
	resp, err := s.do(ctx, method, url, header, body)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, nil, readErr
	}
	return resp.StatusCode, data, nil
}

type streamResponse struct {
	StatusCode int
	Body       io.ReadCloser
}

func (sr *streamResponse) Close() {
	if sr.Body == nil {
		return
	}
	io.Copy(io.Discard, sr.Body)
	sr.Body.Close()
}

func (s *session) doStream(ctx context.Context, method, url string, header http.Header, body io.Reader) (*streamResponse, error) {
	resp, err := s.do(ctx, method, url, header, body)
	if err != nil {
		return nil, err
	}
	return &streamResponse{StatusCode: resp.StatusCode, Body: resp.Body}, nil
}

func (s *session) close() {
	if s.client != nil {
		s.client.CloseIdleConnections()
	}
}

var browserProfiles = []profiles.ClientProfile{profiles.Chrome_124, profiles.Chrome_131}

func pickProfile() profiles.ClientProfile {
	return browserProfiles[rand.Intn(len(browserProfiles))]
}

func createSession(timeoutSec int, proxyURI string) (*session, error) {
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(timeoutSec),
		tls_client.WithClientProfile(pickProfile()),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	}

	if proxyURI != "" {
		if strings.HasPrefix(proxyURI, "http://") || strings.HasPrefix(proxyURI, "https://") || strings.HasPrefix(proxyURI, "socks5://") {
			opts = append(opts, tls_client.WithProxyUrl(proxyURI))
		} else {
			dialCtx, err := proxy.GetDialer(proxyURI)
			if err != nil {
				return nil, fmt.Errorf("node dialer failed: %w", err)
			}
			if dialCtx != nil {
				opts = append(opts, tls_client.WithDialContext(dialCtx))
			}
		}
	}

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, err
	}
	return &session{client: client, proxyURI: proxyURI}, nil
}

func xhrHeaders(contentType, accept, origin, referer, site string) http.Header {
	h := http.Header{
		"sec-ch-ua":          {chUA},
		"sec-ch-ua-mobile":   {"?0"},
		"sec-ch-ua-platform": {`"Windows"`},
		"user-agent":         {userAgent},
		"accept":             {accept},
		"origin":             {origin},
		"sec-fetch-site":     {site},
		"sec-fetch-mode":     {"cors"},
		"sec-fetch-dest":     {"empty"},
		"referer":            {referer},
		"accept-encoding":    {"gzip, deflate, br"},
		"accept-language":    {"en-US,en;q=0.9"},
		"priority":           {"u=1, i"},
		http.HeaderOrderKey: {
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "user-agent",
			"content-type", "accept", "origin", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest",
			"referer", "accept-encoding", "accept-language", "priority",
		},
	}
	if contentType != "" {
		h["content-type"] = []string{contentType}
	}
	return h
}

func anchorHeaders() http.Header {
	return http.Header{
		"sec-ch-ua":                 {chUA},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {`"Windows"`},
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {userAgent},
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
		"sec-fetch-site":            {"cross-site"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-dest":            {"iframe"},
		"accept-encoding":           {"gzip, deflate, br"},
		"accept-language":           {"en-US,en;q=0.9"},
		http.HeaderOrderKey: {
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "upgrade-insecure-requests",
			"user-agent", "accept", "sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest",
			"accept-encoding", "accept-language",
		},
	}
}
