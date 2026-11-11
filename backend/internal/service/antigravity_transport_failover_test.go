package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// upstreamEOF 模拟线上观察到的 `Post ".../v1internal:streamGenerateContent?alt=sse": EOF`。
func upstreamEOF() error {
	return &url.Error{Op: "Post", URL: "https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse", Err: io.EOF}
}

func withSlowConnectionClosedThreshold(t *testing.T, threshold time.Duration) {
	t.Helper()
	prev := antigravitySlowConnectionClosedThreshold
	antigravitySlowConnectionClosedThreshold = threshold
	t.Cleanup(func() { antigravitySlowConnectionClosedThreshold = prev })
}

func newTransportFailoverAccount() *Account {
	return &Account{
		ID:          35,
		Name:        "acc-network-eof",
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "token",
			"project_id":   "proj",
			"model_mapping": map[string]any{
				"gemini-2.5-flash": "gemini-2.5-flash",
			},
		},
	}
}

func newTransportFailoverService(upstream HTTPUpstream) *AntigravityGatewayService {
	return &AntigravityGatewayService{
		settingService: NewSettingService(&antigravitySettingRepoStub{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}),
		tokenProvider:  &AntigravityTokenProvider{},
		httpUpstream:   upstream,
	}
}

func eofErrors(n int) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = upstreamEOF()
	}
	return errs
}

func TestIsAntigravitySlowConnectionClosed(t *testing.T) {
	withSlowConnectionClosedThreshold(t, 30*time.Second)
	require.True(t, isAntigravitySlowConnectionClosed(upstreamEOF(), 72*time.Second))
	require.True(t, isAntigravitySlowConnectionClosed(fmt.Errorf("read: %w", io.ErrUnexpectedEOF), 31*time.Second))
	require.True(t, isAntigravitySlowConnectionClosed(errors.New("read tcp 1.2.3.4:5->6.7.8.9:443: read: connection reset by peer"), time.Minute))
	require.False(t, isAntigravitySlowConnectionClosed(upstreamEOF(), 200*time.Millisecond), "fast EOF (stale keep-alive) keeps same-account retries")
	require.False(t, isAntigravitySlowConnectionClosed(errors.New("dial tcp: lookup failed"), time.Minute))
	require.False(t, isAntigravitySlowConnectionClosed(nil, time.Minute))
}

func TestAntigravityRetryLoop_SlowEOFSkipsSameAccountRetries(t *testing.T) {
	withSlowConnectionClosedThreshold(t, 0)
	upstream := &queuedHTTPUpstreamStub{errors: eofErrors(antigravityMaxRetries)}
	svc := newTransportFailoverService(upstream)

	_, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		ctx:            context.Background(),
		prefix:         "[test]",
		account:        newTransportFailoverAccount(),
		accessToken:    "token",
		action:         "streamGenerateContent",
		body:           []byte(`{}`),
		httpUpstream:   upstream,
		settingService: svc.settingService,
		requestedModel: "gemini-2.5-flash",
	})
	var transportErr *antigravityUpstreamTransportError
	require.ErrorAs(t, err, &transportErr)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 1, upstream.callCount, "a slow EOF must fail over instead of waiting again on the same account")
}

func TestAntigravityRetryLoop_FastEOFStillRetriesSameAccount(t *testing.T) {
	withSlowConnectionClosedThreshold(t, time.Hour)
	upstream := &queuedHTTPUpstreamStub{errors: eofErrors(antigravityMaxRetries)}
	svc := newTransportFailoverService(upstream)

	_, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		ctx:            context.Background(),
		prefix:         "[test]",
		account:        newTransportFailoverAccount(),
		accessToken:    "token",
		action:         "streamGenerateContent",
		body:           []byte(`{}`),
		httpUpstream:   upstream,
		settingService: svc.settingService,
		requestedModel: "gemini-2.5-flash",
	})
	var transportErr *antigravityUpstreamTransportError
	require.ErrorAs(t, err, &transportErr)
	require.Equal(t, antigravityMaxRetries, upstream.callCount)
}

func TestAntigravityGatewayService_ForwardGemini_NetworkErrorTriggersFailover(t *testing.T) {
	withSlowConnectionClosedThreshold(t, 0)
	for _, sticky := range []bool{false, true} {
		t.Run(fmt.Sprintf("sticky=%v", sticky), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			body, err := json.Marshal(map[string]any{
				"contents": []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "hello"}}}},
			})
			require.NoError(t, err)
			c.Request = httptest.NewRequest(http.MethodPost, "/antigravity/v1beta/models/gemini-2.5-flash:streamGenerateContent", bytes.NewReader(body))
			c.Request.Header.Set("User-Agent", "google-genai-sdk/1.71.0 gl-go/go1.28")

			upstream := &queuedHTTPUpstreamStub{errors: eofErrors(antigravityMaxRetries)}
			svc := newTransportFailoverService(upstream)

			result, err := svc.ForwardGemini(context.Background(), c, newTransportFailoverAccount(), "gemini-2.5-flash", "streamGenerateContent", true, body, sticky)
			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr, "network errors must trigger an account switch instead of a direct 502")
			require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
			require.False(t, failoverErr.RetryableOnSameAccount, "same-account retries already happened in the retry loop")
			require.True(t, failoverErr.ShouldRetryNextAccount())
			require.Equal(t, sticky, failoverErr.ForceCacheBilling)
			require.False(t, c.Writer.Written(), "nothing may be written before the handler fails over")
			require.Zero(t, recorder.Body.Len())
		})
	}
}

func TestAntigravityGatewayService_Forward_NetworkErrorTriggersFailover(t *testing.T) {
	withSlowConnectionClosedThreshold(t, 0)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	body, err := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4-5",
		"messages":   []map[string]string{{"role": "user", "content": "hello"}},
		"max_tokens": 16,
	})
	require.NoError(t, err)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	account := newTransportFailoverAccount()
	account.Credentials["model_mapping"] = map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5"}
	upstream := &queuedHTTPUpstreamStub{errors: eofErrors(antigravityMaxRetries)}
	svc := newTransportFailoverService(upstream)

	result, err := svc.Forward(context.Background(), c, account, body, true)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.ForceCacheBilling)
	require.False(t, c.Writer.Written())
}

func TestAntigravityCompatTransportError_NetworkErrorTriggersFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/chat/completions", nil)

	err := (&AntigravityGatewayService{}).handleAntigravityCompatTransportError(c, &antigravityUpstreamTransportError{err: upstreamEOF()})
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.False(t, c.Writer.Written())
	require.Zero(t, recorder.Body.Len())
}

func TestAntigravityTransportFailoverError_Guards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	transportErr := &antigravityUpstreamTransportError{err: upstreamEOF()}

	c, _ := newAntigravityCompatContext(http.MethodPost, "/", nil)
	require.NotNil(t, antigravityTransportFailoverError(c, transportErr, false))
	require.NotNil(t, antigravityTransportFailoverError(nil, transportErr, false))
	require.Nil(t, antigravityTransportFailoverError(c, errors.New("no antigravity forward base url configured"), false), "non-network errors keep the existing response")
	require.Nil(t, antigravityTransportFailoverError(c, &antigravityUpstreamTransportError{err: context.Canceled}, false))

	written, _ := newAntigravityCompatContext(http.MethodPost, "/", nil)
	written.Writer.WriteHeaderNow()
	require.Nil(t, antigravityTransportFailoverError(written, transportErr, false), "a committed response cannot fail over")

	canceled, _ := newAntigravityCompatContext(http.MethodPost, "/", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled.Request = canceled.Request.WithContext(ctx)
	require.Nil(t, antigravityTransportFailoverError(canceled, transportErr, false), "client disconnect must not fail over")
}
