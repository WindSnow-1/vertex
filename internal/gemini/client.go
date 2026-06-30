package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"vertex/internal/config"
	"vertex/internal/metrics"
	"vertex/internal/nodes"
	"vertex/internal/proxypool"
)

const (
	anonBaseURL      = "https://cloudconsole-pa.clients6.google.com"
	batchGraphqlPath = "/v3/entityServices/AiplatformEntityService/schemas/AIPLATFORM_GRAPHQL:batchGraphql"
	anonAPIKey       = "AIzaSyCI-zsRP85UVOi0DjtiCwWBwQ1djDy741g"
)

var batchGraphqlURL = anonBaseURL + batchGraphqlPath + "?key=" + anonAPIKey + "&prettyPrint=false"

var defaultSafetySettings = []any{
	map[string]any{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"},
	map[string]any{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE"},
	map[string]any{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "BLOCK_NONE"},
	map[string]any{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "BLOCK_NONE"},
	map[string]any{"category": "HARM_CATEGORY_CIVIC_INTEGRITY", "threshold": "BLOCK_NONE"},
}

type Client struct {
	cfg        *config.AppConfig
	pool       *tokenPool
	maxRetries int
}

type RequestContext struct {
	Collector *metrics.Collector
}

func NewClient(cfg *config.AppConfig) *Client {
	mr := cfg.MaxRetries
	if mr <= 0 {
		mr = 2
	}
	return &Client{
		cfg:        cfg,
		pool:       newTokenPool(),
		maxRetries: mr,
	}
}

func (c *Client) HandleModels(w http.ResponseWriter, r *http.Request, ctx *RequestContext) {
	now := time.Now().Unix()
	models := config.ModelsWithFakeVariants()
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id": m, "object": "model", "created": now, "owned_by": "google",
		})
	}
	writeJSONHTTP(w, 200, map[string]any{"object": "list", "data": data})
}

func (c *Client) HandleModelInfo(w http.ResponseWriter, r *http.Request, ctx *RequestContext) {
	name := r.URL.Path
	if idx := strings.LastIndex(name, "/models/"); idx >= 0 {
		name = name[idx+8:]
	}
	name = strings.TrimSuffix(name, "/")
	known := false
	for _, m := range config.ModelsWithFakeVariants() {
		if m == name {
			known = true
			break
		}
	}
	if !known {
		writeJSONHTTP(w, 404, map[string]any{"error": map[string]any{
			"code": 404, "message": "Model '" + name + "' not found.", "status": "NOT_FOUND",
		}})
		return
	}
	writeJSONHTTP(w, 200, map[string]any{
		"name":                       "models/" + name,
		"version":                    name,
		"displayName":                name,
		"description":                "Vertex AI Studio anonymous model",
		"inputTokenLimit":            1048576,
		"outputTokenLimit":           65536,
		"supportedGenerationMethods": []any{"generateContent", "streamGenerateContent", "countTokens"},
	})
}

func (c *Client) HandleChatCompletions(w http.ResponseWriter, r *http.Request, ctx *RequestContext) {
	if r.Method != http.MethodPost {
		oaiErrorHTTP(w, 405, "method not allowed", "invalid_request_error")
		return
	}

	ctx.Collector.IncActive()
	defer ctx.Collector.DecActive()
	start := time.Now()

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		oaiErrorHTTP(w, 400, "invalid JSON", "invalid_request_error")
		return
	}

	rawModel, _ := body["model"].(string)
	if strings.TrimSpace(rawModel) == "" {
		oaiErrorHTTP(w, 400, "missing required field 'model'", "invalid_request_error")
		return
	}

	actualModel, useFake := config.StripFakePrefix(rawModel)
	body["model"] = config.ResolveModel(actualModel)

	stream, _ := body["stream"].(bool)
	model, geminiPayload := convertToGemini(body)

	cfg := config.Load()

	if stream && useFake {
		c.oaiFakeStream(r.Context(), w, model, geminiPayload)
		ctx.Collector.IncSuccess()
		ctx.Collector.Record(r.URL.Path, true, time.Since(start).Milliseconds())
		return
	}

	if stream {
		c.handleStreamRequest(w, r, ctx, start, model, geminiPayload, cfg)
	} else {
		c.handleNonStreamRequest(w, r, ctx, start, model, geminiPayload, cfg)
	}
}

func (c *Client) handleNonStreamRequest(w http.ResponseWriter, r *http.Request, ctx *RequestContext, start time.Time, model string, geminiPayload map[string]any, cfg *config.AppConfig) {
	op := func(ctx context.Context, proxyURI string) (map[string]any, error) {
		return c.completeInner(ctx, model, geminiPayload, proxyURI)
	}

	result, err := runParallel(r.Context(), cfg, op)
	if err != nil {
		ve := asVertexError(err)
		ctx.Collector.IncFail()
		ctx.Collector.Record(r.URL.Path, false, time.Since(start).Milliseconds())
		if ve != nil {
			oaiErrorHTTP(w, ve.Code, ve.Message, "api_error")
		} else {
			oaiErrorHTTP(w, 502, err.Error(), "api_error")
		}
		return
	}

	oaiResp := geminiToOAI(result, model)
	ctx.Collector.IncSuccess()
	ctx.Collector.Record(r.URL.Path, true, time.Since(start).Milliseconds())
	writeJSONHTTP(w, 200, oaiResp)
}

func (c *Client) handleStreamRequest(w http.ResponseWriter, r *http.Request, ctx *RequestContext, start time.Time, model string, geminiPayload map[string]any, cfg *config.AppConfig) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		oaiErrorHTTP(w, 500, "streaming not supported", "api_error")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	requestID := generateID()
	isFirst := true

	op := func(ctx context.Context, proxyURI string) <-chan streamChunk {
		ch := make(chan streamChunk, 64)
		go func() {
			defer close(ch)
			c.streamInner(ctx, model, geminiPayload, proxyURI, func(chunk streamChunk) bool {
				select {
				case ch <- chunk:
					return true
				case <-ctx.Done():
					return false
				}
			})
		}()
		return ch
	}

	streamParallel(r.Context(), cfg, op, func(chunk streamChunk) bool {
		if chunk.Err != nil {
			errJSON, _ := json.Marshal(formatOAIError(chunk.Err.Code, chunk.Err.Message, "api_error"))
			fmt.Fprintf(w, "data: %s\n\n", errJSON)
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			ctx.Collector.IncFail()
			ctx.Collector.Record(r.URL.Path, false, time.Since(start).Milliseconds())
			return false
		}

		events := geminiChunkToSSE(chunk.Data, model, requestID, isFirst)
		isFirst = false
		for _, event := range events {
			fmt.Fprint(w, event)
		}
		flusher.Flush()
		return true
	})

	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
	ctx.Collector.IncSuccess()
	ctx.Collector.Record(r.URL.Path, true, time.Since(start).Milliseconds())
}

// --- Non-streaming core ---

func (c *Client) completeInner(ctx context.Context, model string, geminiPayload map[string]any, proxyURI string) (map[string]any, error) {
	maxRetries := c.maxRetries
	recaptchaToken := ""
	isFirstAuth := true
	attempt := 0

	sess, err := createSession(180, proxyURI)
	if err != nil {
		return nil, newInternalError("create session: " + err.Error())
	}
	defer sess.close()

	for attempt <= maxRetries {
		log.Printf("[vertex] attempt %d/%d, model=%s, node=%s", attempt, maxRetries, model, nodes.GetNodeName(proxyURI))

		if recaptchaToken == "" {
			tok, _ := c.pool.getToken(proxyURI)
			recaptchaToken = tok
			isFirstAuth = true
		}
		if recaptchaToken == "" {
			if attempt == maxRetries {
				return nil, newAuthError("could not fetch recaptcha token")
			}
			attempt++
			if err := sleepCtx(ctx, time.Second); err != nil {
				return nil, newInternalError("request canceled")
			}
			continue
		}

		result, reqErr := c.executeCompleteRequest(ctx, sess, model, geminiPayload, recaptchaToken)
		if reqErr == nil {
			if candidateFinish(result) == "SAFETY" {
				if _, hasSafety := geminiPayload["safetySettings"]; !hasSafety {
					retryPayload := shallowCopy(geminiPayload)
					retryPayload["safetySettings"] = defaultSafetySettings
					result, reqErr = c.executeCompleteRequest(ctx, sess, model, retryPayload, recaptchaToken)
				}
			}
		}
		if reqErr == nil {
			return result, nil
		}

		ve := asVertexError(reqErr)
		switch {
		case ve != nil && ve.Kind == "auth":
			if isFirstAuth && isAuthError(ve.Message) {
				isFirstAuth = false
				if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
					return nil, newInternalError("request canceled")
				}
				continue
			}
			recaptchaToken = ""
			isFirstAuth = true
			if attempt < maxRetries {
				attempt++
				if err := sleepCtx(ctx, time.Second); err != nil {
					return nil, newInternalError("request canceled")
				}
				continue
			}
			return nil, ve

		case ve != nil && ve.Kind == "ratelimit":
			if attempt >= maxRetries {
				return nil, ve
			}
			sess.close()
			newSess, e := createSession(180, proxyURI)
			if e != nil {
				return nil, newInternalError("recreate session: " + e.Error())
			}
			sess = newSess
			recaptchaToken = ""
			wait := ve.RetryAfter
			if wait <= 0 {
				wait = min(10, 1+attempt)
			}
			log.Printf("[vertex] 429 retry in %ds, node=%s", wait, nodes.GetNodeName(proxyURI))
			attempt++
			if err := sleepCtx(ctx, time.Duration(wait)*time.Second); err != nil {
				return nil, newInternalError("request canceled")
			}
			continue

		case ve != nil:
			if ve.Kind == "internal" || !ve.IsRetryable() || attempt >= maxRetries {
				return nil, ve
			}
			attempt++
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return nil, newInternalError("request canceled")
			}
			continue

		default:
			return nil, newInternalError(reqErr.Error())
		}
	}
	return nil, newInternalError("all retries exhausted")
}

func (c *Client) executeCompleteRequest(ctx context.Context, sess *session, model string, geminiPayload map[string]any, recaptchaToken string) (map[string]any, error) {
	cfg := c.cfg
	payload := buildRequestPayload(model, geminiPayload, recaptchaToken, cfg)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, newInternalError("marshal payload: " + err.Error())
	}

	header := xhrHeaders(
		"application/json", "*/*",
		"https://console.cloud.google.com", "https://console.cloud.google.com/", "cross-site",
	)

	status, raw, err := sess.doAndRead(ctx, "POST", batchGraphqlURL, header, bytes.NewReader(payloadBytes))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, newInternalError("upstream request: " + err.Error())
	}

	if status != 200 {
		errText := string(raw)
		if status == 401 || status == 403 || isAuthError(errText) {
			return nil, newAuthError("auth failed: " + errText)
		}
		if parsed := parseErrorResponse(errText); parsed != nil {
			return nil, parsed
		}
		return nil, raiseForStatus(status, "", "upstream error: "+errText)
	}

	if len(raw) == 0 {
		return nil, newEmptyResponseError("upstream returned no data")
	}

	result := parseUpstreamData(string(raw))
	if result.HasError && len(result.Parts) == 0 {
		errMsg := result.ErrorMessage
		if isAuthError(errMsg) {
			return nil, newAuthError("auth failed: " + errMsg)
		}
		if result.ErrorObj != nil {
			return nil, result.ErrorObj
		}
		lower := strings.ToLower(errMsg)
		switch {
		case strings.Contains(lower, "not found"):
			return nil, newNotFoundError(errMsg)
		case strings.Contains(lower, "resource has been exhausted") || strings.Contains(lower, "quota"):
			return nil, newRateLimitError(errMsg, 0)
		default:
			return nil, newInvalidArgError(errMsg)
		}
	}

	return buildCompleteResponse(result)
}

func buildCompleteResponse(r *parseResult) (map[string]any, error) {
	if len(r.Parts) == 0 && !r.HasError && len(r.PromptFeedback) == 0 {
		return nil, newEmptyResponseError("upstream returned empty response")
	}

	allParts := r.Parts
	if len(allParts) == 0 {
		allParts = []map[string]any{{"text": " "}}
	}
	partsAny := make([]any, len(allParts))
	for i, p := range allParts {
		partsAny[i] = p
	}

	candidate := map[string]any{
		"content": map[string]any{"parts": partsAny, "role": "model"},
	}
	if r.FinishReason != "" {
		candidate["finishReason"] = strings.ToUpper(r.FinishReason)
	}

	resp := map[string]any{"candidates": []any{candidate}}
	if len(r.PromptFeedback) > 0 {
		resp["promptFeedback"] = r.PromptFeedback
	}
	if len(r.UsageMetadata) > 0 {
		resp["usageMetadata"] = r.UsageMetadata
	}
	if r.ModelVersion != nil {
		resp["modelVersion"] = r.ModelVersion
	}
	if r.ResponseID != nil {
		resp["responseId"] = r.ResponseID
	}
	return resp, nil
}

func candidateFinish(result map[string]any) string {
	if cands, ok := result["candidates"].([]any); ok && len(cands) > 0 {
		if c, ok := cands[0].(map[string]any); ok {
			return toStr(c["finishReason"])
		}
	}
	return ""
}

func chunkHasContent(ch map[string]any) bool {
	cands, ok := ch["candidates"].([]any)
	if !ok || len(cands) == 0 {
		return false
	}
	c, ok := cands[0].(map[string]any)
	if !ok {
		return false
	}
	content, ok := c["content"].(map[string]any)
	if !ok {
		return false
	}
	parts, ok := content["parts"].([]any)
	if !ok {
		return false
	}
	for _, pRaw := range parts {
		p, ok := pRaw.(map[string]any)
		if !ok {
			continue
		}
		if p["thought"] == true {
			continue
		}
		if toStr(p["text"]) != "" {
			return true
		}
		if isFunctionCallWithName(p) {
			return true
		}
	}
	return false
}

// --- Streaming core ---

func (c *Client) streamInner(ctx context.Context, model string, geminiPayload map[string]any, proxyURI string, yield func(streamChunk) bool) {
	maxRetries := c.maxRetries
	contentYielded := false
	var lastError *VertexError

	sess, err := createSession(180, proxyURI)
	if err != nil {
		yield(streamChunk{Err: newInternalError("create session: " + err.Error())})
		return
	}
	defer sess.close()

	recaptchaToken := ""
	isFirstAuth := true
	attempt := 0

	for attempt <= maxRetries {
		log.Printf("[vertex] stream attempt %d/%d, model=%s, node=%s", attempt, maxRetries, model, nodes.GetNodeName(proxyURI))

		if recaptchaToken == "" {
			tok, _ := c.pool.getToken(proxyURI)
			recaptchaToken = tok
			isFirstAuth = true
		}
		if recaptchaToken == "" {
			if attempt == maxRetries {
				lastError = newAuthError("could not fetch recaptcha token")
				break
			}
			attempt++
			if sleepCtx(ctx, time.Second) != nil {
				break
			}
			continue
		}

		chunkCount := 0
		attemptErr := c.executeStreamingAttempt(ctx, sess, model, geminiPayload, recaptchaToken, func(ch map[string]any) bool {
			chunkCount++
			if chunkHasContent(ch) {
				contentYielded = true
			}
			return yield(streamChunk{Data: ch})
		})

		if attemptErr == nil {
			if chunkCount == 0 && isFirstAuth {
				isFirstAuth = false
				if sleepCtx(ctx, 500*time.Millisecond) != nil {
					break
				}
				continue
			}
			return
		}

		ve := asVertexError(attemptErr)
		switch {
		case ve != nil && ve.Kind == "auth":
			if isFirstAuth && isAuthError(ve.Message) {
				isFirstAuth = false
				if sleepCtx(ctx, 500*time.Millisecond) != nil {
					goto done
				}
				continue
			}
			recaptchaToken = ""
			isFirstAuth = true
			lastError = ve
			if contentYielded || attempt >= maxRetries {
				goto done
			}
			attempt++
			if sleepCtx(ctx, time.Second) != nil {
				goto done
			}

		case ve != nil && ve.Kind == "ratelimit":
			lastError = ve
			if contentYielded || attempt >= maxRetries {
				goto done
			}
			sess.close()
			newSess, e := createSession(180, proxyURI)
			if e != nil {
				yield(streamChunk{Err: newInternalError("recreate session: " + e.Error())})
				return
			}
			sess = newSess
			recaptchaToken = ""
			wait := ve.RetryAfter
			if wait <= 0 {
				wait = min(10, 1+attempt)
			}
			attempt++
			if sleepCtx(ctx, time.Duration(wait)*time.Second) != nil {
				goto done
			}

		case ve != nil:
			lastError = ve
			if ve.Kind == "internal" || !ve.IsRetryable() || contentYielded || attempt >= maxRetries {
				goto done
			}
			attempt++
			if sleepCtx(ctx, backoff(attempt)) != nil {
				goto done
			}

		default:
			lastError = newInternalError(attemptErr.Error())
			goto done
		}
	}

done:
	if !contentYielded && lastError != nil {
		yield(streamChunk{Err: lastError})
	}
}

func (c *Client) executeStreamingAttempt(ctx context.Context, sess *session, model string, geminiPayload map[string]any, recaptchaToken string, emit func(map[string]any) bool) error {
	cfg := c.cfg
	payload := buildRequestPayload(model, geminiPayload, recaptchaToken, cfg)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return newInternalError("marshal payload: " + err.Error())
	}

	header := xhrHeaders(
		"application/json", "*/*",
		"https://console.cloud.google.com", "https://console.cloud.google.com/", "cross-site",
	)

	sr, err := sess.doStream(ctx, "POST", batchGraphqlURL, header, bytes.NewReader(payloadBytes))
	if err != nil {
		return newInternalError("upstream request: " + err.Error())
	}
	defer sr.Close()

	if sr.StatusCode != 200 {
		var buf bytes.Buffer
		buf.ReadFrom(sr.Body)
		errText := buf.String()
		if sr.StatusCode == 401 || sr.StatusCode == 403 || isAuthError(errText) {
			return newAuthError("auth failed: " + errText)
		}
		if parsed := parseErrorResponse(errText); parsed != nil {
			return parsed
		}
		return raiseForStatus(sr.StatusCode, "", "upstream error: "+errText)
	}

	return scanStream(sr.Body, func(obj map[string]any) (stop bool, err error) {
		return processStreamingObject(obj, emit)
	})
}

// --- Parallel racing ---

func runParallel[T any](ctx context.Context, cfg *config.AppConfig, op func(ctx context.Context, proxyURI string) (T, error)) (T, error) {
	if cfg.ProxyPoolEnabled {
		proxyURL := proxypool.Next()
		if proxyURL == "" {
			proxyURL = cfg.ProxyURL
		}
		log.Printf("[vertex] using proxy pool: %s", proxyURL)
		return op(ctx, proxyURL)
	}

	cands := nodes.SelectForParallel(cfg.ParallelPoolSize)
	if !cfg.ParallelPoolEnabled || len(cands) == 0 {
		proxy := cfg.ActiveNodeURI
		if proxy == "" {
			proxy = cfg.ProxyURL
		}
		return op(ctx, proxy)
	}

	log.Printf("[vertex] parallel race with %d nodes", len(cands))

	ctxRace, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		uri string
		val T
		err error
	}

	resCh := make(chan result, len(cands))
	var active int32

	for _, cand := range cands {
		atomic.AddInt32(&active, 1)
		go func(u string) {
			v, err := op(ctxRace, u)
			select {
			case resCh <- result{u, v, err}:
			case <-ctxRace.Done():
			}
		}(cand.RawURI)
	}

	var lastErr error
	var zero T
	for atomic.LoadInt32(&active) > 0 {
		select {
		case res := <-resCh:
			atomic.AddInt32(&active, -1)
			name := nodes.GetNodeName(res.uri)
			if res.err == nil {
				log.Printf("[racing] node %s won", name)
				nodes.RecordTest(res.uri, true, 50, "")
				if ctx.Err() == nil {
					return res.val, nil
				}
			} else if !errors.Is(res.err, context.Canceled) {
				log.Printf("[racing] node %s failed: %s", name, res.err.Error())
				nodes.RecordTest(res.uri, false, 0, res.err.Error())
			}
			lastErr = res.err
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}

	if lastErr != nil {
		return zero, lastErr
	}
	return zero, fmt.Errorf("all nodes failed")
}

func streamParallel(ctx context.Context, cfg *config.AppConfig, op func(ctx context.Context, proxyURI string) <-chan streamChunk, yield func(streamChunk) bool) {
	if cfg.ProxyPoolEnabled {
		proxyURL := proxypool.Next()
		if proxyURL == "" {
			proxyURL = cfg.ProxyURL
		}
		log.Printf("[vertex] stream using proxy pool: %s", proxyURL)
		for chunk := range op(ctx, proxyURL) {
			if !yield(chunk) {
				return
			}
		}
		return
	}

	cands := nodes.SelectForParallel(cfg.ParallelPoolSize)
	if !cfg.ParallelPoolEnabled || len(cands) == 0 {
		proxy := cfg.ActiveNodeURI
		if proxy == "" {
			proxy = cfg.ProxyURL
		}
		for chunk := range op(ctx, proxy) {
			if !yield(chunk) {
				return
			}
		}
		return
	}

	log.Printf("[vertex] stream parallel with %d nodes", len(cands))
	ctxRace, cancel := context.WithCancel(ctx)
	defer cancel()

	type res struct {
		uri   string
		ch    <-chan streamChunk
		first streamChunk
		err   error
	}

	resCh := make(chan res, len(cands))
	var active int32

	for _, cand := range cands {
		atomic.AddInt32(&active, 1)
		go func(u string) {
			ch := op(ctxRace, u)
			first, ok := <-ch
			if !ok {
				resCh <- res{u, nil, streamChunk{}, fmt.Errorf("stream closed")}
			} else if first.Err != nil {
				resCh <- res{u, nil, streamChunk{}, first.Err}
			} else {
				resCh <- res{u, ch, first, nil}
			}
		}(cand.RawURI)
	}

	var winner *res
loop:
	for atomic.LoadInt32(&active) > 0 {
		select {
		case r := <-resCh:
			atomic.AddInt32(&active, -1)
			name := nodes.GetNodeName(r.uri)
			if r.err == nil && winner == nil {
				winner = &r
				log.Printf("[racing] stream node %s won", name)
				nodes.RecordTest(r.uri, true, 50, "")
				break loop
			} else if r.err != nil && ctx.Err() == nil && !errors.Is(r.err, context.Canceled) {
				log.Printf("[racing] stream node %s failed: %s", name, r.err.Error())
				nodes.RecordTest(r.uri, false, 0, r.err.Error())
			}
		case <-ctx.Done():
			return
		}
	}
	if winner != nil {
		if !yield(winner.first) {
			return
		}
		for chunk := range winner.ch {
			if !yield(chunk) {
				return
			}
		}
	} else {
		yield(streamChunk{Err: newInternalError("all nodes failed to stream")})
	}
}

// --- Helpers ---

func backoff(attempt int) time.Duration {
	v := math.Pow(1.5, float64(attempt))
	if v > 15 {
		v = 15
	}
	return time.Duration(v * float64(time.Second))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func writeJSONHTTP(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func oaiErrorHTTP(w http.ResponseWriter, status int, message, errType string) {
	log.Printf("[vertex] error: %s", message)
	writeJSONHTTP(w, status, formatOAIError(status, message, errType))
}
