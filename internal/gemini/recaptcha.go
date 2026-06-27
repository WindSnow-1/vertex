package gemini

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"vertex/internal/config"
)

const (
	recaptchaBase = "https://www.google.com"
	siteKey       = "6LdCjtspAAAAAMcV4TGdWLJqRTEk1TfpdLqEnKdj"
	recaptchaCo   = "aHR0cHM6Ly9jb25zb2xlLmNsb3VkLmdvb2dsZS5jb206NDQz"
	recaptchaHl   = "zh-CN"
	recaptchaV    = "jdMmXeCQEkPbnFDy9T04NbgJ"
	recaptchaVh   = "6581054572"
	randomCharset = "abcdefghijklmnopqrstuvwxyz0123456789"
)

var (
	tokenRe = regexp.MustCompile(`id="recaptcha-token"[^>]*value="([^"]+)"`)
	rrespRe = regexp.MustCompile(`rresp","(.*?)"`)
)

func randomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = randomCharset[rand.Intn(len(randomCharset))]
	}
	return string(b)
}

func fetchRecaptchaToken(proxyURI string) (string, error) {
	start := time.Now()
	for retry := 0; retry < 3; retry++ {
		log.Printf("[recaptcha] fetching token (attempt %d/3)", retry+1)
		if token, ok := fetchTokenOnce(proxyURI); ok {
			log.Printf("[recaptcha] got token in %dms", time.Since(start).Milliseconds())
			return token, nil
		}
	}
	log.Printf("[recaptcha] failed after 3 attempts (%dms)", time.Since(start).Milliseconds())
	return "", nil
}

func fetchTokenOnce(proxyURI string) (string, bool) {
	sess, err := createSession(15, proxyURI)
	if err != nil {
		return "", false
	}
	defer sess.close()

	cb := randomString(10)
	anchorURL := fmt.Sprintf(
		"%s/recaptcha/enterprise/anchor?ar=1&k=%s&co=%s&hl=%s&v=%s&size=invisible&anchor-ms=20000&execute-ms=15000&cb=%s",
		recaptchaBase, siteKey, recaptchaCo, recaptchaHl, recaptchaV, cb,
	)

	_, anchorBody, err := sess.doAndRead(context.Background(), "GET", anchorURL, anchorHeaders(), nil)
	if err != nil {
		return "", false
	}
	m := tokenRe.FindSubmatch(anchorBody)
	if m == nil {
		bodyStr := string(anchorBody)
		if len(bodyStr) > 500 {
			bodyStr = bodyStr[:500] + "..."
		}
		log.Printf("[recaptcha] anchor token regex failed, body: %s", bodyStr)
		return "", false
	}
	baseToken := string(m[1])

	form := url.Values{
		"v":      {recaptchaV},
		"reason": {"q"},
		"k":      {siteKey},
		"c":      {baseToken},
		"co":     {recaptchaCo},
		"hl":     {recaptchaHl},
		"size":   {"invisible"},
		"vh":     {recaptchaVh},
		"chr":    {""},
		"bg":     {""},
	}
	reloadURL := recaptchaBase + "/recaptcha/enterprise/reload?k=" + siteKey
	header := xhrHeaders(
		"application/x-www-form-urlencoded;charset=UTF-8", "*/*",
		recaptchaBase, anchorURL, "same-origin",
	)

	status, reloadBody, err := sess.doAndRead(context.Background(), "POST", reloadURL, header, strings.NewReader(form.Encode()))
	if err != nil {
		return "", false
	}
	if status != 200 {
		log.Printf("[recaptcha] reload failed, status: %d", status)
	}
	rm := rrespRe.FindSubmatch(reloadBody)
	if rm == nil {
		return "", false
	}
	return string(rm[1]), true
}

type tokenPool struct {
	mu sync.Mutex
}

func newTokenPool() *tokenPool {
	return &tokenPool{}
}

func (p *tokenPool) getToken(proxyURI string) (string, error) {
	if proxyURI == "" {
		proxyURI = config.Load().ProxyURL
	}
	return fetchRecaptchaToken(proxyURI)
}
