package zonneplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Client is a Zonneplan API client. It is safe for concurrent use.
type Client struct {
	http       *http.Client
	base       *url.URL
	maxRetries int

	email     string
	token     *Token
	mu        sync.Mutex // guards token
	refreshMu sync.Mutex // serializes refresh grants (the refresh token rotates)
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient uses the given http.Client instead of the default (10s timeout).
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// WithBaseURL overrides the API base url, e.g. for tests.
func WithBaseURL(s string) Option {
	return func(c *Client) {
		u, err := url.Parse(s)
		if err != nil {
			panic(fmt.Sprintf("invalid base url: %v", err))
		}
		c.base = u
	}
}

// WithMaxRetries sets how often failed GET requests are retried. Default 3.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// NewClient returns a Zonneplan API client.
func NewClient(opts ...Option) *Client {
	c := &Client{
		http:       &http.Client{Timeout: 10 * time.Second},
		base:       mustURL(APIURL),
		maxRetries: 3,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(fmt.Sprintf("invalid url: %v", err))
	}
	return u
}

// requestOpts carries per-request behaviour.
type requestOpts struct {
	// allowForbidden treats a 403 response as success. Zonneplan's OTP
	// challenge endpoint returns 403 on the happy path.
	allowForbidden bool

	// authenticated attaches the bearer token, refreshing it when it is about
	// to expire, and selects the 400 mapping: a data endpoint rejecting its
	// parameters is a request error, while an unauthenticated call (the oauth
	// endpoints themselves) rejects an expired or invalid grant with 400.
	authenticated bool
}

// do performs a request and returns the JSON-decoded response body.
// Only GET requests are retried, and only on connection errors, timeouts
// and 5xx responses: repeating a POST or PUT could apply an action twice.
func (c *Client) do(ctx context.Context, method, uri string, params url.Values, body any, opts requestOpts) (json.RawMessage, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrRequest, method, err)
		}
	}

	u, err := c.base.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid uri %q: %v", ErrRequest, uri, err)
	}
	if len(params) > 0 {
		u.RawQuery = params.Encode()
	}
	if opts.authenticated {
		if err := c.ensureToken(ctx); err != nil {
			return nil, err
		}
	}
	token := c.currentToken()

	attempts := 1
	if method == http.MethodGet {
		attempts = c.maxRetries + 1
	}

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
		}

		raw, err := c.attempt(ctx, method, u, payload, token, opts)
		if err == nil {
			return raw, nil
		}
		if attempt+1 >= attempts || !retryable(err) {
			return nil, err
		}
	}
}

// attempt performs a single request/response roundtrip.
func (c *Client) attempt(ctx context.Context, method string, u *url.URL, payload []byte, token *Token, opts requestOpts) (json.RawMessage, error) {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %v", ErrRequest, method, u, err)
	}
	req.Header.Set("content-type", "application/json;charset=utf-8")
	req.Header.Set("user-agent", "go-zonneplan/"+Version)
	req.Header.Set("x-app-version", appVersion)
	req.Header.Set("x-app-environment", appEnvironment)
	if token != nil && token.AccessToken != "" {
		req.Header.Set("authorization", "Bearer "+token.AccessToken)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, wrapTransportError(method, u, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: reading %s %s: %v", ErrConnection, method, u, err)
	}

	if (resp.StatusCode < 200 || resp.StatusCode > 299) && resp.StatusCode != http.StatusNotModified {
		if opts.allowForbidden && resp.StatusCode == http.StatusForbidden {
			return body, nil
		}
		return nil, statusError(method, u, resp, body, opts.authenticated)
	}

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified || len(body) == 0 {
		return nil, nil
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("%w: %s %s: %s", ErrResponse, method, u, snippet(body))
	}
	return body, nil
}

// wrapTransportError maps a transport failure to ErrTimeout or ErrConnection.
// The inner error stays in the chain (errors.Is/As), so callers can classify
// wrapped causes such as a missing login.
func wrapTransportError(method string, u *url.URL, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return fmt.Errorf("%w: %s %s: %w", ErrTimeout, method, u, err)
	}
	return fmt.Errorf("%w: %s %s: %w", ErrConnection, method, u, err)
}

func isTimeout(err error) bool {
	var ne netError
	return errors.As(err, &ne) && ne.Timeout()
}

type netError interface {
	Timeout() bool
}

// statusError maps a non-2xx response to the appropriate error.
func statusError(method string, u *url.URL, resp *http.Response, body []byte, authenticated bool) error {
	sn := snippet(body)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		// Retry-After may also be an HTTP date; only the seconds form is used.
		e := &RateLimitError{}
		if s := resp.Header.Get("Retry-After"); s != "" {
			if secs, err := strconv.Atoi(s); err == nil && secs > 0 {
				e.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		return e
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s %s: %s", ErrNotFound, method, u, sn)
	case resp.StatusCode == http.StatusBadRequest && !authenticated:
		// An oauth endpoint rejecting its grant answers with a plain 400.
		return fmt.Errorf("%w: %s %s: %s", ErrAuth, method, u, sn)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s %s: %s", ErrAuth, method, u, sn)
	case resp.StatusCode < 500:
		return fmt.Errorf("%w: %s %s: %s", ErrRequest, method, u, sn)
	default:
		return fmt.Errorf("%w: %s %s: %s", ErrConnection, method, u, sn)
	}
}

func snippet(body []byte) string {
	const max = 200
	if len(body) > max {
		return string(body[:max]) + "..."
	}
	return string(body)
}

// retryBaseDelay is the first backoff interval; doubled each attempt.
var retryBaseDelay = time.Second

// backoff returns the wait before the given retry attempt: exponential
// from 1s, capped at 10s, mirroring the reference client.
func backoff(attempt int) time.Duration {
	d := retryBaseDelay << (attempt - 1)
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
