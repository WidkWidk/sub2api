package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiClientRejectsSSEComments(t *testing.T) {
	cases := []struct {
		name string
		hint string
		want bool
	}{
		{"go-genai (Antigravity CLI)", "google-genai-sdk/1.71.0 gl-go/go1.28-20260721-RC03 cl/951519500 +3ebc191975 X:fieldtrack,boringcrypto", true},
		{"python-genai", "google-genai-sdk/1.20.0 gl-python/3.12.4", true},
		{"js-genai tolerates comments", "google-genai-sdk/1.9.0 gl-node/22.3.0", false},
		{"gemini-cli", "GeminiCLI/0.60.0 (darwin; arm64)", false},
		{"curl", "curl/8.7.1", false},
		{"empty", "", false},
		{"case-insensitive", "Google-GenAI-SDK/1.0.0 GL-Go/go1.27", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, geminiClientRejectsSSEComments(tc.hint))
		})
	}
}

func TestDownstreamRejectsSSECommentsReadsBothHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := newAntigravityCompatContext(http.MethodPost, "/v1beta/models/gemini-3.8-flash:streamGenerateContent", nil)
	require.False(t, downstreamRejectsSSEComments(c))

	c.Request.Header.Set("X-Goog-Api-Client", "google-genai-sdk/1.71.0 gl-go/go1.28")
	require.True(t, downstreamRejectsSSEComments(c))

	c.Request.Header.Del("X-Goog-Api-Client")
	c.Request.Header.Set("User-Agent", "google-genai-sdk/1.71.0 gl-go/go1.28")
	require.True(t, downstreamRejectsSSEComments(c))

	require.False(t, downstreamRejectsSSEComments(nil))
}

// runAntigravityGeminiStreamWithIdle 起一条上游流：先发一个 data 事件，然后空闲 idle 时长再关闭，
// 返回写给下游的全部字节。用来观察空闲期间网关是否发了 ":\n\n" 心跳。
func runAntigravityGeminiStreamWithIdle(t *testing.T, userAgent string, idle time.Duration) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(
		config.GatewayConfig{MaxLineSize: defaultMaxLineSize, StreamKeepaliveInterval: 1},
		nil,
	)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1beta/models/gemini-3.8-flash:streamGenerateContent", nil)
	if userAgent != "" {
		c.Request.Header.Set("User-Agent", userAgent)
	}
	reader, writer := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
	done := make(chan error, 1)
	go func() {
		_, err := svc.handleGeminiStreamingResponse(c, resp, time.Now())
		done <- err
	}()
	_, err := io.WriteString(
		writer,
		`data: {"response":{"responseId":"resp_1","candidates":[{"content":{"parts":[{"text":"partial"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1}}}`+"\n\n",
	)
	require.NoError(t, err)
	time.Sleep(idle)
	require.NoError(t, writer.Close())
	require.NoError(t, <-done)
	require.NoError(t, reader.Close())
	return recorder.Body.String()
}

func TestAntigravityGeminiStreamKeepsCommentKeepaliveForOrdinaryClients(t *testing.T) {
	// The keepalive interval is configured in whole seconds. Leave enough idle
	// time for the first data event to be processed before the ticker fires;
	// otherwise a busy CI runner can consume the first tick too early and close
	// the stream before any heartbeat is written.
	out := runAntigravityGeminiStreamWithIdle(t, "curl/8.7.1", 2200*time.Millisecond)
	require.Contains(t, out, ":\n\n", "ordinary clients should still get the idle keepalive")
	require.Contains(t, out, `"text":"partial"`)
}

func TestAntigravityGeminiStreamSkipsCommentKeepaliveForGoGenai(t *testing.T) {
	out := runAntigravityGeminiStreamWithIdle(t, "google-genai-sdk/1.71.0 gl-go/go1.28-20260721-RC03", 1200*time.Millisecond)
	require.Contains(t, out, `"text":"partial"`)
	for _, event := range strings.Split(out, "\n\n") {
		require.False(t, strings.HasPrefix(event, ":"), "go-genai must never receive an SSE comment event, got %q", event)
	}
}

func TestGeminiGoogleStreamErrorEvent(t *testing.T) {
	cases := []struct {
		reason string
		code   int
		status string
	}{
		{"stream_read_error", http.StatusServiceUnavailable, "UNAVAILABLE"},
		{"stream_timeout", http.StatusServiceUnavailable, "UNAVAILABLE"},
		{"response_too_large", http.StatusInternalServerError, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			event := geminiGoogleStreamErrorEvent(tc.reason)
			require.True(t, strings.HasPrefix(event, "data: "))
			require.True(t, strings.HasSuffix(event, "\n\n"))
			require.Equal(t, 1, strings.Count(event, "\n\n"))
			got := decodeGoogleStreamError(t, strings.TrimSuffix(strings.TrimPrefix(event, "data: "), "\n\n"))
			require.Equal(t, tc.code, got.Code)
			require.Equal(t, tc.status, got.Status)
			require.Equal(t, tc.reason, got.Message)
		})
	}
}

type googleStreamErrorInfo struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func decodeGoogleStreamError(t *testing.T, payload string) googleStreamErrorInfo {
	t.Helper()
	var wrapper struct {
		Error *googleStreamErrorInfo `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(payload), &wrapper))
	require.NotNil(t, wrapper.Error, "payload must carry an error object: %s", payload)
	return *wrapper.Error
}

// runAntigravityGeminiStreamWithReadError 起一条上游流：可选地先发一个 data 事件，然后以读错误中断，
// 返回下游收到的字节、handler 返回的错误以及 gin.Context。
func runAntigravityGeminiStreamWithReadError(t *testing.T, userAgent string, sendFirstChunk bool) (string, error, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, nil)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1beta/models/gemini-3.8-flash:streamGenerateContent", nil)
	if userAgent != "" {
		c.Request.Header.Set("User-Agent", userAgent)
	}
	reader, writer := io.Pipe()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}
	go func() {
		if sendFirstChunk {
			_, _ = io.WriteString(
				writer,
				`data: {"response":{"responseId":"resp_1","candidates":[{"content":{"parts":[{"text":"partial"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":1}}}`+"\n\n",
			)
		}
		_ = writer.CloseWithError(io.ErrUnexpectedEOF)
	}()
	_, err := svc.handleGeminiStreamingResponse(c, resp, time.Now())
	_ = reader.Close()
	return recorder.Body.String(), err, c
}

func TestAntigravityGeminiStreamReadErrorUsesGoogleErrorChunkForGoGenai(t *testing.T) {
	out, err, _ := runAntigravityGeminiStreamWithReadError(t, "google-genai-sdk/1.71.0 gl-go/go1.28-20260721-RC03", true)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "partial stream must not be retried by failover")

	require.Contains(t, out, `"text":"partial"`)
	require.NotContains(t, out, "event:")
	events := strings.Split(strings.TrimSuffix(out, "\n\n"), "\n\n")
	for _, event := range events {
		require.True(t, strings.HasPrefix(event, "data:"), "go-genai only accepts data events, got %q", event)
	}
	last := events[len(events)-1]
	got := decodeGoogleStreamError(t, strings.TrimSpace(strings.TrimPrefix(last, "data:")))
	require.Equal(t, http.StatusServiceUnavailable, got.Code)
	require.Equal(t, "UNAVAILABLE", got.Status)
	require.Equal(t, "stream_read_error", got.Message)
}

func TestAntigravityGeminiStreamReadErrorKeepsNamedErrorEventForOrdinaryClients(t *testing.T) {
	out, err, _ := runAntigravityGeminiStreamWithReadError(t, "curl/8.7.1", true)
	require.Error(t, err)
	require.Contains(t, out, `"text":"partial"`)
	require.True(t, strings.HasSuffix(out, "event: error\ndata: {\"error\":\"stream_read_error\"}\n\n"), "got %q", out)
}

func TestAntigravityGeminiStreamReadErrorBeforeFirstByteTriggersFailover(t *testing.T) {
	for _, ua := range []string{"google-genai-sdk/1.71.0 gl-go/go1.28", "curl/8.7.1"} {
		t.Run(ua, func(t *testing.T) {
			out, err, c := runAntigravityGeminiStreamWithReadError(t, ua, false)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
			require.True(t, failoverErr.RetryableOnSameAccount)
			require.Empty(t, out)
			require.False(t, c.Writer.Written(), "response must stay uncommitted so failover can retry")
			require.Empty(t, c.Writer.Header().Get("Content-Type"), "stream headers must be reset for the failover/error response")
		})
	}
}

func TestAntigravityGeminiStreamTimeoutUsesGoogleErrorChunkForGoGenai(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newAntigravityCompatService(
		config.GatewayConfig{MaxLineSize: defaultMaxLineSize, StreamDataIntervalTimeout: 1},
		nil,
	)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1beta/models/gemini-3.8-flash:streamGenerateContent", nil)
	c.Request.Header.Set("X-Goog-Api-Client", "google-genai-sdk/1.71.0 gl-go/go1.28")
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
	go func() {
		_, _ = io.WriteString(
			writer,
			`data: {"response":{"responseId":"resp_1","candidates":[{"content":{"parts":[{"text":"partial"}]}}]}}`+"\n\n",
		)
	}()
	_, err := svc.handleGeminiStreamingResponse(c, resp, time.Now())
	_ = reader.Close()
	require.Error(t, err)

	out := recorder.Body.String()
	require.NotContains(t, out, "event:")
	events := strings.Split(strings.TrimSuffix(out, "\n\n"), "\n\n")
	last := events[len(events)-1]
	require.True(t, strings.HasPrefix(last, "data:"), "got %q", last)
	got := decodeGoogleStreamError(t, strings.TrimSpace(strings.TrimPrefix(last, "data:")))
	require.Equal(t, http.StatusServiceUnavailable, got.Code)
	require.Equal(t, "stream_timeout", got.Message)
}
