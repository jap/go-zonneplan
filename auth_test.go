package zonneplan

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tokenResponse = `{"access_token": "at-1", "refresh_token": "rt-1", "expires_in": 3600, "token_type": "Bearer"}`

func TestPKCE(t *testing.T) {
	verifier, challenge, err := generatePKCEPair()
	require.NoError(t, err)

	assert.Len(t, verifier, 128)
	assert.Regexp(t, `^[A-Za-z0-9_-]+$`, verifier)
	assert.Regexp(t, `^[A-Za-z0-9_-]+$`, challenge)
	assert.Len(t, challenge, 43) // base64url of sha256, no padding

	verifier2, challenge2, err := generatePKCEPair()
	require.NoError(t, err)
	assert.NotEqual(t, verifier, verifier2)
	assert.NotEqual(t, challenge, challenge2)
}

func TestRequestOTP(t *testing.T) {
	var body map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/"+challengePath, r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"otp_required": true, "auth_session": "sess-1"}`))
	})
	c.email = "user@example.com"

	challenge, err := c.RequestOTP(context.Background(), "evcc")
	require.NoError(t, err)

	assert.Equal(t, "sess-1", challenge.AuthSession)
	assert.Equal(t, "code", body["response_type"])
	assert.Equal(t, "user@example.com", body["email"])
	assert.Equal(t, "S256", body["code_challenge_method"])
	assert.NotEmpty(t, body["code_challenge"])
	// the challenge is the hash of the verifier carried back
	sum := sha256.Sum256([]byte(challenge.CodeVerifier))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), body["code_challenge"])
}

func TestRequestOTPNoEmail(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})

	_, err := c.RequestOTP(context.Background(), "evcc")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAuth)
}

func TestRequestOTPUnexpectedResponse(t *testing.T) {
	for _, body := range []string{`{}`, `{"otp_required": true}`, `{"auth_session": "sess-1"}`, ``} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(body))
		})
		c.email = "user@example.com"

		_, err := c.RequestOTP(context.Background(), "evcc")
		require.Error(t, err, "body %q", body)
		assert.ErrorIs(t, err, ErrAuth, "body %q", body)
	}
}

func TestSubmitOTP(t *testing.T) {
	var tokenGrant map[string]string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		switch {
		case r.URL.Path == "/"+challengePath && body["otp"] != "":
			_, _ = w.Write([]byte(`{"authorization_code": "code-1"}`))
		case r.URL.Path == "/"+challengePath:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"otp_required": true, "auth_session": "sess-1"}`))
		case r.URL.Path == "/"+tokenPath:
			tokenGrant = body
			_, _ = w.Write([]byte(tokenResponse))
		}
	})
	c.email = "user@example.com"

	challenge, err := c.RequestOTP(context.Background(), "evcc")
	require.NoError(t, err)

	require.NoError(t, c.SubmitOTP(context.Background(), challenge, "123456"))
	assert.Equal(t, "authorization_code", tokenGrant["grant_type"])
	assert.Equal(t, "code-1", tokenGrant["code"])
	assert.Equal(t, challenge.CodeVerifier, tokenGrant["code_verifier"])

	token := c.Token()
	require.NotNil(t, token)
	assert.Equal(t, "at-1", token.AccessToken)
	assert.Equal(t, "rt-1", token.RefreshToken)
	assert.WithinDuration(t, time.Now().Add(time.Hour), token.Expiry, 5*time.Second)
}

func TestSubmitOTPRejected(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message": "invalid otp"}`))
	})

	err := c.SubmitOTP(context.Background(), OtpChallenge{AuthSession: "sess-1", CodeVerifier: "v"}, "000000")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAuth)
	assert.Contains(t, err.Error(), "invalid otp")
}

func TestRefreshToken(t *testing.T) {
	var grant map[string]string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/"+tokenPath, r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&grant))
		_, _ = w.Write([]byte(`{"access_token": "at-2", "refresh_token": "rt-2", "expires_in": 3600}`))
	})
	c.SetToken(&Token{AccessToken: "at-1", RefreshToken: "rt-1", Expiry: time.Now().Add(time.Hour)})

	require.NoError(t, c.RefreshToken(context.Background()))
	assert.Equal(t, "rt-1", grant["refresh_token"])

	token := c.Token()
	assert.Equal(t, "at-2", token.AccessToken)
	assert.Equal(t, "rt-2", token.RefreshToken, "rotation must be stored")
}

func TestRefreshWithoutLogin(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {})

	err := c.RefreshToken(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAuth)
}

func TestTransparentRefresh(t *testing.T) {
	var tokenCalls, dataCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + tokenPath:
			tokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token": "at-2", "refresh_token": "rt-2", "expires_in": 3600}`))
		default:
			dataCalls.Add(1)
			// both attempts (one retry) must carry the refreshed token
			assert.Equal(t, "Bearer at-2", r.Header.Get("Authorization"), r.URL.Path)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL+"/"), WithMaxRetries(1))
	// expired: transparent refresh before the request
	c.SetToken(&Token{AccessToken: "at-1", RefreshToken: "rt-1", Expiry: time.Now().Add(-time.Minute)})

	_, err := c.do(context.Background(), http.MethodGet, "api/test", nil, nil, requestOpts{authenticated: true})
	require.NoError(t, err)
	assert.EqualValues(t, 1, tokenCalls.Load())
	assert.EqualValues(t, 1, dataCalls.Load())
}

func TestNoRefreshWhenValid(t *testing.T) {
	var tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + tokenPath:
			tokenCalls.Add(1)
			_, _ = w.Write([]byte(tokenResponse))
		default:
			assert.Equal(t, "Bearer at-1", r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL + "/"))
	c.SetToken(&Token{AccessToken: "at-1", RefreshToken: "rt-1", Expiry: time.Now().Add(time.Hour)})

	_, err := c.do(context.Background(), http.MethodGet, "api/test", nil, nil, requestOpts{authenticated: true})
	require.NoError(t, err)
	assert.EqualValues(t, 0, tokenCalls.Load())
}

func TestTokenValid(t *testing.T) {
	assert.False(t, (*Token)(nil).Valid())
	assert.False(t, (&Token{}).Valid())
	assert.False(t, (&Token{AccessToken: "at", Expiry: time.Now().Add(30 * time.Second)}).Valid(), "within expiry margin")
	assert.True(t, (&Token{AccessToken: "at", Expiry: time.Now().Add(2 * time.Minute)}).Valid())
}

func TestTokenJSONRoundTrip(t *testing.T) {
	in := Token{AccessToken: "at", TokenType: "Bearer", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour).Truncate(time.Second)}

	data, err := json.Marshal(&in)
	require.NoError(t, err)

	var out Token
	require.NoError(t, json.Unmarshal(data, &out))
	assert.Equal(t, in, out)
}
