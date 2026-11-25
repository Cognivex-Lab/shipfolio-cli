package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCallSendsKeyAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sf_live_x" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "shipfolio-cli/test" {
			t.Errorf("User-Agent = %q", got)
		}
		w.Write([]byte(`{"status":"ok","data":{"workspace_name":"Acme","plan":"free","scopes":["metrics:read"]}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL + "/", Key: "sf_live_x", UserAgent: "shipfolio-cli/test"}
	var out Envelope[Whoami]
	if err := c.Call(context.Background(), "GET", "/v1/whoami", nil, nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Data.WorkspaceName != "Acme" || out.Data.Scopes[0] != "metrics:read" {
		t.Fatalf("decoded %+v", out.Data)
	}
}

func TestErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"status":"error","error":{"code":"unauthorized","message":"invalid API key","retryable":false}}`))
	}))
	defer srv.Close()
	_, err := (&Client{BaseURL: srv.URL}).Do(context.Background(), "GET", "/v1/whoami", nil, nil)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "unauthorized" || ae.Status != 401 {
		t.Fatalf("err = %#v", err)
	}
}

func TestNonJSONError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>bad gateway</html>"))
	}))
	defer srv.Close()
	_, err := (&Client{BaseURL: srv.URL}).Do(context.Background(), "GET", "/v1/x", nil, nil)
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 502 || !ae.Retryable {
		t.Fatalf("err = %#v", err)
	}
}

func TestRetriesRateLimited(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"status":"error","error":{"code":"rate_limited","message":"slow down","retryable":true}}`))
			return
		}
		w.Write([]byte(`{"status":"ok","data":[]}`))
	}))
	defer srv.Close()
	var waited []time.Duration
	c := &Client{BaseURL: srv.URL, Sleep: func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }}
	if _, err := c.Do(context.Background(), "GET", "/v1/products", nil, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(waited) != 2 || waited[0] != time.Second {
		t.Fatalf("calls=%d waited=%v", calls, waited)
	}
}

func TestQuotaIsNotRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"status":"error","error":{"code":"quota_exhausted","message":"monthly allowance used","retryable":false}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Sleep: func(context.Context, time.Duration) error { return nil }}
	if _, err := c.Do(context.Background(), "GET", "/v1/products", nil, nil); err == nil {
		t.Fatal("want an error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestTokenRenewedOnceAfter401(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer sfo_new" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"status":"error","error":{"code":"unauthorized","message":"sign in","retryable":false}}`))
			return
		}
		w.Write([]byte(`{"status":"ok","data":[]}`))
	}))
	defer srv.Close()
	var renews []bool
	c := &Client{BaseURL: srv.URL, Token: func(_ context.Context, renew bool) (string, error) {
		renews = append(renews, renew)
		if renew {
			return "sfo_new", nil
		}
		return "sfo_old", nil
	}}
	if _, err := c.Do(context.Background(), "GET", "/v1/products", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "Bearer sfo_old" || len(renews) != 2 || !renews[1] {
		t.Fatalf("seen=%v renews=%v", seen, renews)
	}
	// A token that is still refused after renewing is not renewed again.
	c.Token = func(context.Context, bool) (string, error) { return "sfo_bad", nil }
	seen = nil
	_, err := c.Do(context.Background(), "GET", "/v1/products", nil, nil)
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != "unauthorized" || len(seen) != 2 {
		t.Fatalf("err=%v seen=%v", err, seen)
	}
}
