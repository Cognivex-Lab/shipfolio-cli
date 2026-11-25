package cli

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Cognivex-Lab/shipfolio-cli/internal/client"
)

// browserTimeout is how long login --browser waits for the person to
// sign in and approve.
const browserTimeout = 5 * time.Minute

// loginBrowser signs in through the API's OAuth server: it registers the
// CLI as a public client with a loopback redirect, sends the person to the
// consent page with PKCE, and catches the code on 127.0.0.1. The tokens it
// gets can only read (metrics:read).
func (a *App) loginBrowser(ctx context.Context, c *common, cfg config) error {
	o := a.oauthClient()
	base := a.apiURL(c, cfg)
	meta, err := o.Discover(ctx, base)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen for the browser's answer: %w", err)
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	// The port changes every time, so the CLI registers afresh each login.
	clientID, err := o.Register(ctx, meta.RegistrationEndpoint, "Shipfolio CLI", []string{redirect})
	if err != nil {
		return fmt.Errorf("register the CLI with %s: %w", base, err)
	}

	state := client.RandomString(24)
	verifier := client.RandomString(32)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"scope":                 {"metrics:read"},
		"state":                 {state},
		"code_challenge":        {client.Challenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(meta.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	authURL := meta.AuthorizationEndpoint + sep + q.Encode()

	type answer struct {
		code string
		err  error
	}
	answers := make(chan answer, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query()
		var ans answer
		switch {
		case p.Get("state") != state:
			ans.err = errors.New("the browser's answer does not belong to this login (the state differs); run 'shipfolio login --browser' again")
		case p.Get("error") != "":
			msg := p.Get("error_description")
			if msg == "" {
				msg = p.Get("error")
			}
			ans.err = fmt.Errorf("the sign-in was not approved: %s", msg)
		case p.Get("iss") != "" && meta.Issuer != "" && p.Get("iss") != meta.Issuer:
			ans.err = fmt.Errorf("the answer came from %s, not %s", p.Get("iss"), meta.Issuer)
		case p.Get("code") == "":
			ans.err = errors.New("the browser's answer has no authorization code")
		default:
			ans.code = p.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		msg := "Signed in to Shipfolio. You can close this tab and go back to the terminal."
		if ans.err != nil {
			w.WriteHeader(http.StatusBadRequest)
			msg = "Shipfolio sign-in failed: " + ans.err.Error()
		}
		fmt.Fprintf(w, "<!DOCTYPE html><title>Shipfolio CLI</title><p style=\"font:16px system-ui;margin:3em\">%s</p>\n", html.EscapeString(msg))
		select {
		case answers <- ans:
		default: // only the first answer counts
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	fmt.Fprintf(a.Stderr, "Opening your browser to sign in. If it does not open, visit:\n\n  %s\n\n", authURL)
	if a.OpenBrowser != nil {
		if err := a.OpenBrowser(authURL); err != nil {
			fmt.Fprintf(a.Stderr, "Could not open a browser (%v); open the address above.\n", err)
		}
	}
	wait, cancel := context.WithTimeout(ctx, browserTimeout)
	defer cancel()
	var ans answer
	select {
	case ans = <-answers:
	case <-wait.Done():
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("gave up waiting for the browser sign-in; run 'shipfolio login --browser' again")
	}
	if ans.err != nil {
		return ans.err
	}

	t, err := o.ExchangeCode(ctx, meta.TokenEndpoint, clientID, ans.code, redirect, verifier)
	if err != nil {
		return fmt.Errorf("finish the sign-in: %w", err)
	}
	var who client.Envelope[client.Whoami]
	if err := a.newClient(c, t.AccessToken, cfg).Call(ctx, "GET", "/v1/whoami", nil, nil, &who); err != nil {
		return err
	}
	a.revokeSaved(ctx, cfg.OAuth)
	cfg.APIKey = ""
	cfg.OAuth = &oauthLogin{
		ClientID:       clientID,
		AccessToken:    t.AccessToken,
		RefreshToken:   t.RefreshToken,
		Expiry:         t.Expiry(a.now()),
		TokenEndpoint:  meta.TokenEndpoint,
		RevokeEndpoint: meta.RevocationEndpoint,
	}
	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Signed in to %s (%s plan) through the browser. The sign-in is saved in %s.\n", who.Data.WorkspaceName, who.Data.Plan, a.configPath())
	fmt.Fprintln(a.Stdout, "It can read data only. To add, change or delete, use an API key with 'shipfolio login'.")
	if a.Getenv("SHIPFOLIO_API_KEY") != "" {
		fmt.Fprintln(a.Stderr, "SHIPFOLIO_API_KEY is set and is used instead of the browser sign-in.")
	}
	return nil
}

// revokeSaved ends a saved browser sign-in on the server. It reports
// whether that worked; a sign-in it cannot end still expires on its own.
func (a *App) revokeSaved(ctx context.Context, login *oauthLogin) bool {
	if login == nil || login.RevokeEndpoint == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// Revoking the refresh token ends the grant, and with it the access token.
	tok := login.RefreshToken
	if tok == "" {
		tok = login.AccessToken
	}
	return a.oauthClient().Revoke(ctx, login.RevokeEndpoint, login.ClientID, tok) == nil
}
