package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth talks to the API's OAuth 2.1 authorization server: discovery
// (RFC 8414), dynamic client registration (RFC 7591), the authorization
// code flow with PKCE, refresh and revocation (RFC 7009).
//
// Its errors are *Error with Code set to the OAuth error ("invalid_grant")
// and Message to its description. They never include a token.
type OAuth struct {
	HTTP      *http.Client
	UserAgent string
}

// AuthServer is the authorization server metadata the CLI uses.
type AuthServer struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
}

// Token is a token endpoint response.
type Token struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// Expiry is when the access token stops working, from now.
func (t Token) Expiry(now time.Time) time.Time {
	if t.ExpiresIn <= 0 {
		return now.Add(time.Hour)
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}

// Discover reads /.well-known/oauth-authorization-server under baseURL.
func (o *OAuth) Discover(ctx context.Context, baseURL string) (AuthServer, error) {
	var m AuthServer
	u := strings.TrimRight(baseURL, "/") + "/.well-known/oauth-authorization-server"
	if err := o.send(ctx, "GET", u, "", nil, &m); err != nil {
		return m, err
	}
	for _, e := range []string{m.AuthorizationEndpoint, m.TokenEndpoint, m.RegistrationEndpoint} {
		p, err := url.Parse(e)
		if e == "" || err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
			return m, fmt.Errorf("%s does not offer sign-in through the browser", baseURL)
		}
	}
	return m, nil
}

// Register registers a public client (PKCE only, no secret) and returns
// its client_id.
func (o *OAuth) Register(ctx context.Context, endpoint, name string, redirectURIs []string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                name,
		"redirect_uris":              redirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
	})
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := o.send(ctx, "POST", endpoint, "application/json", body, &out); err != nil {
		return "", err
	}
	if out.ClientID == "" {
		return "", fmt.Errorf("the server registered no client_id")
	}
	return out.ClientID, nil
}

// ExchangeCode trades an authorization code for tokens.
func (o *OAuth) ExchangeCode(ctx context.Context, tokenURL, clientID, code, redirectURI, verifier string) (Token, error) {
	return o.token(ctx, tokenURL, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
}

// Refresh trades a refresh token for new tokens. Refresh tokens rotate:
// the returned one replaces the one given, which stops working.
func (o *OAuth) Refresh(ctx context.Context, tokenURL, clientID, refreshToken string) (Token, error) {
	return o.token(ctx, tokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	})
}

// Revoke ends the grant behind an access or refresh token.
func (o *OAuth) Revoke(ctx context.Context, revokeURL, clientID, token string) error {
	form := url.Values{"token": {token}, "client_id": {clientID}}
	return o.send(ctx, "POST", revokeURL, "application/x-www-form-urlencoded", []byte(form.Encode()), nil)
}

func (o *OAuth) token(ctx context.Context, tokenURL string, form url.Values) (Token, error) {
	var t Token
	if err := o.send(ctx, "POST", tokenURL, "application/x-www-form-urlencoded", []byte(form.Encode()), &t); err != nil {
		return t, err
	}
	if t.AccessToken == "" {
		return t, fmt.Errorf("the server returned no access token")
	}
	return t, nil
}

func (o *OAuth) send(ctx context.Context, method, u, contentType string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if o.UserAgent != "" {
		req.Header.Set("User-Agent", o.UserAgent)
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg := e.Description
			if msg == "" {
				msg = "the sign-in failed"
			}
			return &Error{Status: resp.StatusCode, Code: e.Error, Message: msg}
		}
		return decodeError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unexpected response from %s: %w", req.URL.Path, err)
	}
	return nil
}

// RandomString returns n random bytes, base64url encoded without padding:
// for state values and PKCE verifiers.
func RandomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Challenge is the PKCE S256 code challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
