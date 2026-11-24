package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAPI serves a small workspace. Calls records "METHOD path?query".
type fakeAPI struct {
	*httptest.Server
	calls []string
	key   string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{key: "sf_live_good"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			call += "?" + r.URL.RawQuery
		}
		f.calls = append(f.calls, call)
		if r.Header.Get("Authorization") != "Bearer "+f.key {
			w.WriteHeader(401)
			io.WriteString(w, `{"status":"error","error":{"code":"unauthorized","message":"invalid API key","retryable":false}}`)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/whoami":
			io.WriteString(w, `{"status":"ok","data":{"workspace_id":"wsp_1","workspace_name":"Acme","plan":"free","scopes":["metrics:read","sync:write"]}}`)
		case "GET /v1/products":
			io.WriteString(w, `{"status":"ok","data":[
				{"id":"prd_a","name":"Alpha","domain":"alpha.com","sources":[{"id":"src_1","type":"gsc","external_id":"sc-domain:alpha.com","status":"ok"}]},
				{"id":"prd_b","name":"Beta","domain":"beta.io","sources":[]}]}`)
		case "GET /v1/overview":
			io.WriteString(w, `{"status":"ok","as_of":"2026-11-20","data":{"has_data":true,
				"period":{"start":"2026-10-24","end":"2026-11-20","prev_start":"2026-09-26","prev_end":"2026-10-23","days":28},
				"products":[
				{"id":"prd_a","name":"Alpha","domain":"alpha.com","current":{"clicks":12345,"impressions":400000,"ctr":0.0308,"avg_position":7.25,"sessions":null},"change":{"clicks":0.125,"impressions":-0.04,"sessions":null}},
				{"id":"prd_b","name":"Beta","domain":"beta.io","current":{"clicks":0,"sessions":null},"change":{}}]}}`)
		case "GET /v1/metrics":
			io.WriteString(w, `{"status":"ok","start":"2026-11-19","end":"2026-11-20","data":[{"product_id":"prd_a","points":[
				{"date":"2026-11-19","values":{"clicks":10,"sessions":null},"is_final":true},
				{"date":"2026-11-20","values":{"clicks":0,"sessions":null},"is_final":false}]}]}`)
		case "GET /v1/metrics/breakdown":
			io.WriteString(w, `{"status":"ok","data":{"product_id":"prd_a","dimension":"query","sorted_by":"clicks","start":"2026-10-24","end":"2026-11-20",
				"rows":[{"value":"alpha app","clicks":50,"impressions":900,"ctr":0.0556,"avg_position":2.1,"sessions":null}]}}`)
		case "DELETE /v1/products/prd_b":
			io.WriteString(w, `{"status":"ok","data":{"deleted":true}}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"status":"error","error":{"code":"not_found","message":"not found","retryable":false}}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

type run struct {
	code           int
	stdout, stderr string
}

func newApp(t *testing.T, env map[string]string, stdin string, interactive bool) *App {
	dir := t.TempDir()
	return &App{
		Stdin:  strings.NewReader(stdin),
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Getenv: func(k string) string {
			if k == "SHIPFOLIO_CONFIG" {
				if v, ok := env[k]; ok {
					return v
				}
				return filepath.Join(dir, "config.json")
			}
			return env[k]
		},
		Interactive: func() bool { return interactive },
		ReadSecret:  func(string) (string, error) { return stdin, nil },
	}
}

func (a *App) exec(args ...string) run {
	code := a.Run(context.Background(), args)
	r := run{code: code, stdout: a.Stdout.(*bytes.Buffer).String(), stderr: a.Stderr.(*bytes.Buffer).String()}
	a.Stdout.(*bytes.Buffer).Reset()
	a.Stderr.(*bytes.Buffer).Reset()
	return r
}

func TestLoginSavesKeyPrivately(t *testing.T) {
	api := newFakeAPI(t)
	app := newApp(t, nil, "sf_live_good\n", false)
	r := app.exec("login", "--api-url", api.URL)
	if r.code != 0 || !strings.Contains(r.stdout, "Signed in to Acme") {
		t.Fatalf("login: %+v", r)
	}
	path := app.Getenv("SHIPFOLIO_CONFIG")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode %v, want 0600", fi.Mode().Perm())
	}
	var cfg config
	raw, _ := os.ReadFile(path)
	_ = json.Unmarshal(raw, &cfg)
	if cfg.APIKey != "sf_live_good" || cfg.APIURL != api.URL {
		t.Fatalf("saved %+v", cfg)
	}
	// The saved key and URL are used afterwards.
	if r := app.exec("whoami"); r.code != 0 || !strings.Contains(r.stdout, "metrics:read, sync:write") {
		t.Fatalf("whoami: %+v", r)
	}
	if r := app.exec("logout"); r.code != 0 {
		t.Fatalf("logout: %+v", r)
	}
	if r := app.exec("whoami"); r.code != 1 || !strings.Contains(r.stderr, "no API key") {
		t.Fatalf("whoami after logout: %+v", r)
	}
}

func TestLoginRejectsBadKeyAndKeepsNothing(t *testing.T) {
	api := newFakeAPI(t)
	app := newApp(t, nil, "sf_live_bad", false)
	r := app.exec("login", "--api-url", api.URL)
	if r.code != 1 || !strings.Contains(r.stderr, "unauthorized") {
		t.Fatalf("login: %+v", r)
	}
	if _, err := os.Stat(app.Getenv("SHIPFOLIO_CONFIG")); !os.IsNotExist(err) {
		t.Fatalf("config written for a bad key: %v", err)
	}
}

func TestLoginRefusesKeyAsArgument(t *testing.T) {
	app := newApp(t, nil, "", false)
	if r := app.exec("login", "sf_live_good"); r.code != 2 {
		t.Fatalf("%+v", r)
	}
}

func keyed(t *testing.T, api *fakeAPI) *App {
	return newApp(t, map[string]string{"SHIPFOLIO_API_KEY": "sf_live_good", "SHIPFOLIO_API_URL": api.URL}, "", false)
}

func TestOverviewShowsUnknownAsDash(t *testing.T) {
	api := newFakeAPI(t)
	r := keyed(t, api).exec("overview", "--period", "28d")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"12,345", "+12.5%", "-4.0%", "3.1%", "7.2", "Data through 2026-11-20"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("overview lacks %q:\n%s", want, r.stdout)
		}
	}
	// Beta reported 0 clicks and no sessions: 0 stays 0, null is a dash.
	var beta string
	for _, line := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(line, "beta.io") {
			beta = line
		}
	}
	fields := strings.Fields(beta)
	if len(fields) < 9 || fields[1] != "0" || fields[7] != none {
		t.Fatalf("beta row = %q", beta)
	}
	if got := api.calls[len(api.calls)-1]; got != "GET /v1/overview?period=last_28_days" {
		t.Fatalf("call = %q", got)
	}
}

func TestMetricsResolvesDomainAndMarksPending(t *testing.T) {
	api := newFakeAPI(t)
	r := keyed(t, api).exec("metrics", "https://www.alpha.com/", "--metrics", "clicks,sessions", "--period", "7d")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.stdout, "2026-11-20*") || !strings.Contains(r.stdout, "may still revise") {
		t.Fatalf("pending day not marked:\n%s", r.stdout)
	}
	last := api.calls[len(api.calls)-1]
	if !strings.Contains(last, "product_ids=prd_a") || !strings.Contains(last, "period=last_7_days") {
		t.Fatalf("call = %q", last)
	}
}

func TestMetricsCSVLeavesUnknownBlank(t *testing.T) {
	api := newFakeAPI(t)
	r := keyed(t, api).exec("metrics", "Alpha", "--metrics", "clicks,sessions", "--csv")
	want := "product_id,date,clicks,sessions,is_final\nprd_a,2026-11-19,10,,true\nprd_a,2026-11-20,0,,false\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("got %q", r.stdout)
	}
}

func TestBreakdownHidesEmptyColumns(t *testing.T) {
	api := newFakeAPI(t)
	r := keyed(t, api).exec("breakdown", "alpha.com", "query", "--limit", "5")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.stdout, "QUERY") || !strings.Contains(r.stdout, "5.6%") || strings.Contains(r.stdout, "SESSIONS") {
		t.Fatalf("breakdown:\n%s", r.stdout)
	}
}

func TestJSONPrintsRawResponse(t *testing.T) {
	api := newFakeAPI(t)
	r := keyed(t, api).exec("whoami", "--json")
	var v struct {
		Status string `json:"status"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &v) != nil || v.Status != "ok" {
		t.Fatalf("%+v", r)
	}
}

func TestDeleteNeedsConfirmation(t *testing.T) {
	api := newFakeAPI(t)
	app := keyed(t, api)
	if r := app.exec("products", "rm", "beta.io"); r.code != 2 || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("without --yes: %+v", r)
	}
	for _, c := range api.calls {
		if strings.HasPrefix(c, "DELETE") {
			t.Fatal("deleted without confirmation")
		}
	}
	if r := app.exec("products", "rm", "beta.io", "--yes"); r.code != 0 || !strings.Contains(r.stdout, "Deleted beta.io") {
		t.Fatalf("with --yes: %+v", r)
	}
}

func TestInteractiveDeleteCanBeCancelled(t *testing.T) {
	api := newFakeAPI(t)
	app := newApp(t, map[string]string{"SHIPFOLIO_API_KEY": "sf_live_good", "SHIPFOLIO_API_URL": api.URL}, "n\n", true)
	if r := app.exec("products", "rm", "Beta"); r.code != 1 || !strings.Contains(r.stderr, "cancelled") {
		t.Fatalf("%+v", r)
	}
	for _, c := range api.calls {
		if strings.HasPrefix(c, "DELETE") {
			t.Fatal("deleted after the answer was no")
		}
	}
}

func TestUnknownProduct(t *testing.T) {
	api := newFakeAPI(t)
	r := keyed(t, api).exec("sync", "gamma.dev")
	if r.code != 1 || !strings.Contains(r.stderr, `no product matches "gamma.dev"`) {
		t.Fatalf("%+v", r)
	}
}

func TestRawAPIPrintsErrorBody(t *testing.T) {
	api := newFakeAPI(t)
	r := keyed(t, api).exec("api", "/v1/nope?x=1")
	if r.code != 1 || !strings.Contains(r.stdout, `"not_found"`) || api.calls[0] != "GET /v1/nope?x=1" {
		t.Fatalf("%+v calls=%v", r, api.calls)
	}
}

func TestUsageErrors(t *testing.T) {
	app := newApp(t, nil, "", false)
	for _, args := range [][]string{{"nope"}, {"metrics"}, {"overview", "--period", "3d"}, {"metrics", "a", "--start", "2026-01-01"}} {
		if r := app.exec(args...); r.code != 2 {
			t.Errorf("%v: code %d, stderr %q", args, r.code, r.stderr)
		}
	}
}
