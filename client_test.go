package zonneplan

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient returns a client pointed at srv with fast retries.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	retryBaseDelay = time.Millisecond
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(WithBaseURL(srv.URL+"/"), WithMaxRetries(2), WithHTTPClient(srv.Client()))
}

func TestErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		opts   requestOpts
		expect error
	}{
		{"not found", http.StatusNotFound, requestOpts{}, ErrNotFound},
		{"unauthorized", http.StatusUnauthorized, requestOpts{}, ErrAuth},
		{"forbidden", http.StatusForbidden, requestOpts{}, ErrAuth},
		{"bad request (data endpoint)", http.StatusBadRequest, requestOpts{authenticated: true}, ErrRequest},
		{"bad request (oauth endpoint)", http.StatusBadRequest, requestOpts{}, ErrAuth},
		{"internal server error", http.StatusInternalServerError, requestOpts{}, ErrConnection},
		{"bad gateway", http.StatusBadGateway, requestOpts{}, ErrConnection},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message": "nope"}`))
			})

			_, err := c.do(context.Background(), http.MethodGet, "api/test", nil, nil, tc.opts)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.expect)
		})
	}
}

func TestForbiddenHappyPath(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"otp_required": true, "auth_session": "sess-1"}`))
	})

	raw, err := c.do(context.Background(), http.MethodPost, "oauth/authorize-challenge", nil, map[string]any{"email": "x"}, requestOpts{allowForbidden: true})
	require.NoError(t, err)

	var res struct {
		OtpRequired bool   `json:"otp_required"`
		AuthSession string `json:"auth_session"`
	}
	require.NoError(t, json.Unmarshal(raw, &res))
	assert.True(t, res.OtpRequired)
}

func TestRateLimit(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, err := c.do(context.Background(), http.MethodGet, "api/test", nil, nil, requestOpts{})
	require.Error(t, err)

	var rle *RateLimitError
	require.ErrorAs(t, err, &rle)
	assert.Equal(t, 5*time.Second, rle.RetryAfter)
	// a rate limit is permanent, not retried
	assert.False(t, retryable(err))
}

func TestRetryGet(t *testing.T) {
	requests := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ok": true}`))
	})

	raw, err := c.do(context.Background(), http.MethodGet, "api/test", nil, nil, requestOpts{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok": true}`, string(raw))
	assert.Equal(t, 3, requests)
}

func TestNoRetryPost(t *testing.T) {
	requests := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := c.do(context.Background(), http.MethodPost, "api/test", nil, map[string]any{}, requestOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConnection)
	assert.Equal(t, 1, requests)
}

func TestRetryExhausted(t *testing.T) {
	requests := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	})

	_, err := c.do(context.Background(), http.MethodGet, "api/test", nil, nil, requestOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConnection)
	assert.Equal(t, 3, requests) // maxRetries(2) + 1
}

func TestRequestHeaders(t *testing.T) {
	var got http.Header
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	})

	_, err := c.do(context.Background(), http.MethodGet, "api/test", nil, nil, requestOpts{})
	require.NoError(t, err)

	assert.Equal(t, "go-zonneplan/"+Version, got.Get("User-Agent"))
	assert.Equal(t, appVersion, got.Get("X-App-Version"))
	assert.Equal(t, appEnvironment, got.Get("X-App-Environment"))
	assert.Equal(t, "application/json;charset=utf-8", got.Get("Content-Type"))
}

func TestQueryParams(t *testing.T) {
	var query string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{}`))
	})

	_, err := c.do(context.Background(), http.MethodGet, "api/test", map[string][]string{"date": {"2026-11-20"}}, nil, requestOpts{})
	require.NoError(t, err)
	assert.Equal(t, "date=2026-11-20", query)
}

func TestInvalidJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})

	_, err := c.do(context.Background(), http.MethodGet, "api/test", nil, nil, requestOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrResponse)
}

func TestEmptyResponse(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		})

		raw, err := c.do(context.Background(), http.MethodGet, "api/test", nil, nil, requestOpts{})
		require.NoError(t, err)
		assert.Nil(t, raw)
	}
}

func TestTimeout(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		time.Sleep(100 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL+"/"), WithMaxRetries(1), WithHTTPClient(&http.Client{Timeout: 20 * time.Millisecond}))

	_, err := c.do(context.Background(), http.MethodGet, "api/test", nil, nil, requestOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTimeout)
	assert.Equal(t, 2, requests)
}

func TestContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})

	_, err := c.do(ctx, http.MethodGet, "api/test", nil, nil, requestOpts{})
	require.Error(t, err)
}

func TestBackoff(t *testing.T) {
	retryBaseDelay = time.Second
	t.Cleanup(func() { retryBaseDelay = time.Millisecond })

	assert.Equal(t, time.Second, backoff(1))
	assert.Equal(t, 2*time.Second, backoff(2))
	assert.Equal(t, 4*time.Second, backoff(3))
	assert.Equal(t, 10*time.Second, backoff(5))
}
