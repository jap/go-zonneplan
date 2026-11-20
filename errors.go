package zonneplan

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors returned by the client. They are wrapped with request
// context, so check with errors.Is. Rate limits return *RateLimitError,
// check with errors.As.
var (
	ErrAuth       = errors.New("zonneplan: authentication failed")
	ErrNotFound   = errors.New("zonneplan: not found")
	ErrRequest    = errors.New("zonneplan: request rejected")
	ErrRateLimit  = errors.New("zonneplan: rate limit hit")
	ErrConnection = errors.New("zonneplan: connection error")
	ErrTimeout    = errors.New("zonneplan: timeout")
	ErrResponse   = errors.New("zonneplan: invalid response")
)

// RateLimitError is returned when the API answers with 429. RetryAfter is
// the seconds form of the Retry-After header, or 0 when absent.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%v: retry after %s", ErrRateLimit, e.RetryAfter)
	}
	return ErrRateLimit.Error()
}

// retryable reports whether the error may go away on a retry. Only
// connection and timeout errors qualify, and only GET requests are retried.
func retryable(err error) bool {
	return errors.Is(err, ErrConnection) || errors.Is(err, ErrTimeout)
}
