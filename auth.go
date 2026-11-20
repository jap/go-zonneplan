package zonneplan

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	challengePath = "oauth/authorize-challenge"
	tokenPath     = "oauth/token"

	// expiryMargin refreshes the access token a little before it actually expires.
	expiryMargin = 60 * time.Second
)

// Token is an access token with its refresh token. The json tags match
// oauth2.Token's layout so evcc can map between the two 1:1.
type Token struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry"`
}

// Valid reports whether the token can be used.
func (t *Token) Valid() bool {
	return t != nil && t.AccessToken != "" && !t.expired()
}

// expired reports whether the token needs refreshing.
func (t *Token) expired() bool {
	return time.Now().After(t.Expiry.Add(-expiryMargin))
}

// OtpChallenge carries the state between requesting and submitting the
// one-time password. Zonneplan mails the OTP to the account address.
type OtpChallenge struct {
	AuthSession  string
	CodeVerifier string
}

// generatePKCEPair returns a fresh (code_verifier, code_challenge) pair:
// a 128-char urlsafe verifier and its base64url SHA-256, per RFC 7636 S256.
func generatePKCEPair() (verifier, challenge string, err error) {
	buf := make([]byte, 96)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generating pkce verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// WithEmail sets the account email used for the OTP login.
func WithEmail(email string) Option {
	return func(c *Client) { c.email = email }
}

// SetToken restores a previously obtained token, e.g. from config. The token
// rotates on refresh: persist the one returned by Token() after every refresh.
func (c *Client) SetToken(t *Token) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = t
}

// Token returns the current token, or nil before login. The pointer is
// invalidated by the next refresh; copy it if it must outlive that.
func (c *Client) Token() *Token {
	return c.currentToken()
}

// RequestOTP starts the login and has Zonneplan mail a one-time password to
// the configured email address. The happy path responds with HTTP 403.
// sourceName labels the session, truncated to 255 characters like the app does.
func (c *Client) RequestOTP(ctx context.Context, sourceName string) (OtpChallenge, error) {
	if c.email == "" {
		return OtpChallenge{}, fmt.Errorf("%w: no email configured", ErrAuth)
	}

	verifier, challenge, err := generatePKCEPair()
	if err != nil {
		return OtpChallenge{}, err
	}
	if runes := []rune(sourceName); len(runes) > 255 {
		sourceName = string(runes[:255])
	}

	body := map[string]string{
		"response_type":         "code",
		"email":                 c.email,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"source_name":           sourceName,
	}
	raw, err := c.do(ctx, http.MethodPost, challengePath, nil, body, requestOpts{allowForbidden: true})
	if err != nil {
		return OtpChallenge{}, err
	}

	var res struct {
		OtpRequired bool   `json:"otp_required"`
		AuthSession string `json:"auth_session"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || !res.OtpRequired || res.AuthSession == "" {
		return OtpChallenge{}, fmt.Errorf("%w: zonneplan did not return an OTP challenge", ErrAuth)
	}
	return OtpChallenge{AuthSession: res.AuthSession, CodeVerifier: verifier}, nil
}

// SubmitOTP exchanges the mailed one-time password for a token and stores it
// on the client. A rejected or expired OTP returns an error wrapping ErrAuth.
func (c *Client) SubmitOTP(ctx context.Context, challenge OtpChallenge, otp string) error {
	raw, err := c.do(ctx, http.MethodPost, challengePath, nil, map[string]string{
		"auth_session": challenge.AuthSession,
		"otp":          otp,
	}, requestOpts{})
	if err != nil {
		return fmt.Errorf("zonneplan rejected the one-time password: %w", err)
	}

	var res struct {
		AuthorizationCode string `json:"authorization_code"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.AuthorizationCode == "" {
		return fmt.Errorf("zonneplan rejected the one-time password: %w", ErrAuth)
	}

	return c.requestToken(ctx, map[string]string{
		"grant_type":    "authorization_code",
		"code":          res.AuthorizationCode,
		"code_verifier": challenge.CodeVerifier,
	})
}

// RefreshToken exchanges the current refresh token for a new one, regardless
// of whether it has expired yet. The refresh token rotates, so the token
// returned by Token() must be persisted after this call.
func (c *Client) RefreshToken(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	if tok := c.currentToken(); tok == nil || tok.RefreshToken == "" {
		return fmt.Errorf("%w: cannot refresh a token before logging in", ErrAuth)
	}
	return c.refreshToken(ctx)
}

// ensureToken refreshes an expiring token before an authenticated request.
func (c *Client) ensureToken(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	if tok := c.currentToken(); tok == nil || !tok.expired() {
		return nil
	}
	return c.refreshToken(ctx)
}

func (c *Client) refreshToken(ctx context.Context) error {
	tok := c.currentToken()
	if tok == nil {
		return fmt.Errorf("%w: cannot refresh a token before logging in", ErrAuth)
	}
	return c.requestToken(ctx, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": tok.RefreshToken,
	})
}

// requestToken calls the token endpoint and stores the result.
func (c *Client) requestToken(ctx context.Context, grant map[string]string) error {
	raw, err := c.do(ctx, http.MethodPost, tokenPath, nil, grant, requestOpts{})
	if err != nil {
		return err
	}

	var res struct {
		Token
		ExpiresIn int64 `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("%w: unexpected token response: %v", ErrResponse, err)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		return fmt.Errorf("%w: token response missing access or refresh token", ErrResponse)
	}
	res.Expiry = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = &res.Token
	return nil
}

// currentToken snapshots the token under the client lock.
func (c *Client) currentToken() *Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}
