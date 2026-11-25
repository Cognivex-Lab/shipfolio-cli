package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cognivex-Lab/shipfolio-cli/internal/client"
)

// fakeAPI serves a small workspace and an OAuth server. Calls records
// "METHOD path?query"; bodies keeps the last request body per "METHOD path".
type fakeAPI struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []string
	bodies map[string]string
	key    string
	// OAuth state: the registered redirect, the PKCE challenge, the
	// current tokens of the one grant, and what was revoked.
	redirect, challenge string
	access, refresh     string
	issued              int
	revoked             []string
	tamperState         bool
}

func (f *fakeAPI) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAPI) lastCall() string {
	c := f.Calls()
	return c[len(c)-1]
}

func (f *fakeAPI) body(call string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[call]
}

func (f *fakeAPI) set(fn func(f *fakeAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"status":"error","error":{"code":%q,"message":%q,"retryable":false}}`, code, msg)
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{key: "sf_live_good", bodies: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		call := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			call += "?" + r.URL.RawQuery
		}
		f.calls = append(f.calls, call)
		if r.Header.Get("Content-Type") == "application/json" {
			b, _ := io.ReadAll(r.Body)
			f.bodies[r.Method+" "+r.URL.Path] = string(b)
		}
		if f.oauth(w, r) {
			return
		}
		readOnly := false
		switch r.Header.Get("Authorization") {
		case "Bearer " + f.key:
		case "Bearer " + f.access:
			readOnly = f.access != ""
		default:
			apiError(w, 401, "unauthorized", "invalid API key")
			return
		}
		if readOnly && (r.Method != "GET" || r.URL.Path == "/v1/products/import") {
			apiError(w, 403, "insufficient_scope", "this key lacks the sync:write scope")
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/whoami":
			scopes := `["metrics:read","sync:write"]`
			if readOnly {
				scopes = `["metrics:read"]`
			}
			io.WriteString(w, `{"status":"ok","data":{"workspace_id":"wsp_1","workspace_name":"Acme","plan":"free","scopes":`+scopes+`}}`)
		case "GET /v1/products":
			io.WriteString(w, `{"status":"ok","data":[
				{"id":"prd_a","name":"Alpha","domain":"alpha.com","sources":[{"id":"src_1","type":"gsc","external_id":"sc-domain:alpha.com","status":"ok"}]},
				{"id":"prd_b","name":"Beta","domain":"beta.io","sources":[]}]}`)
		case "GET /v1/overview":
			compare := r.URL.Query().Get("compare")
			if compare == "" {
				compare = "previous"
			}
			io.WriteString(w, `{"status":"ok","as_of":"2026-11-20","data":{"has_data":true,
				"period":{"start":"2026-10-24","end":"2026-11-20","prev_start":"2026-09-26","prev_end":"2026-10-23","days":28,"compare":"`+compare+`"},
				"products":[
				{"id":"prd_a","name":"Alpha","domain":"alpha.com","current":{"clicks":12345,"impressions":400000,"ctr":0.0308,"avg_position":7.25,"sessions":null,"bing_clicks":321,"bing_impressions":9000},"change":{"clicks":0.125,"impressions":-0.04,"sessions":null,"bing_clicks":0.5}},
				{"id":"prd_b","name":"Beta","domain":"beta.io","current":{"clicks":0,"sessions":null,"bing_clicks":null},"change":{}}]}}`)
		case "GET /v1/metrics":
			io.WriteString(w, `{"status":"ok","start":"2026-11-19","end":"2026-11-20","data":[{"product_id":"prd_a","points":[
				{"date":"2026-11-19","values":{"clicks":10,"sessions":null},"is_final":true},
				{"date":"2026-11-20","values":{"clicks":0,"sessions":null},"is_final":false}]}]}`)
		case "GET /v1/metrics/breakdown":
			if r.URL.Query().Get("dimension") == "bing_query" {
				io.WriteString(w, `{"status":"ok","data":{"product_id":"prd_a","dimension":"bing_query","sorted_by":"bing_clicks","start":"2026-10-24","end":"2026-11-20",
					"rows":[{"value":"alpha app","bing_clicks":12,"bing_impressions":300,"bing_ctr":0.04,"bing_avg_position":3.2,"clicks":null,"sessions":null}]}}`)
				return
			}
			io.WriteString(w, `{"status":"ok","data":{"product_id":"prd_a","dimension":"query","sorted_by":"clicks","start":"2026-10-24","end":"2026-11-20",
				"rows":[{"value":"alpha app","clicks":50,"impressions":900,"ctr":0.0556,"avg_position":2.1,"sessions":null}]}}`)
		case "GET /v1/metrics/movers":
			io.WriteString(w, `{"status":"ok","data":{"product_id":"prd_a","dimension":"query","metric":"clicks",
				"period":{"start":"2026-10-24","end":"2026-11-20","prev_start":"2025-10-24","prev_end":"2025-11-20","days":28,"compare":"year"},
				"gainers":[{"value":"alpha app","current":1200,"previous":1000,"delta":200,"change":0.2},{"value":"alpha new","current":30,"previous":0,"delta":30,"change":null}],
				"losers":[{"value":"alpha old","current":5,"previous":50,"delta":-45,"change":-0.9}]}}`)
		case "GET /v1/tasks":
			io.WriteString(w, `{"status":"ok","data":[
				{"id":"tsk_1","product_id":"prd_a","title":"OAI-SearchBot is blocked in robots.txt","detail":"ChatGPT cannot read the site.","fix":"User-agent: OAI-SearchBot\nAllow: /","source":"audit","level":"fail","url":"https://alpha.com/robots.txt","status":"open","created_at":"2026-11-23T00:00:00Z"},
				{"id":"tsk_2","product_id":"prd_a","title":"Rewrite the pricing title","source":"opportunity","query":"alpha pricing","status":"done","done_on":"2026-10-01","created_at":"2026-09-30T00:00:00Z",
				 "outcome":{"basis":"query","ready":true,"days_after":28,"before":{"clicks":100,"impressions":1000},"after":{"clicks":150,"impressions":1200},"clicks_change":0.5}}]}`)
		case "PATCH /v1/tasks/tsk_1":
			io.WriteString(w, `{"status":"ok","data":{"id":"tsk_1","product_id":"prd_a","title":"OAI-SearchBot is blocked in robots.txt","status":"done","done_on":"2026-11-25"}}`)
		case "POST /v1/tasks":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"status":"ok","data":{"id":"tsk_9","product_id":"prd_a","title":"x","status":"open"}}`)
		case "GET /v1/site-audit", "POST /v1/products/prd_a/site-audit":
			io.WriteString(w, `{"status":"ok","data":{"product_id":"prd_a","domain":"alpha.com","audit":{"checked_at":"2026-11-23T10:00:00Z","robots_status":200,"score":57,
				"crawlers":[{"agent":"OAI-SearchBot","purpose":"search","allowed":false},{"agent":"Googlebot","purpose":"search","allowed":true}],
				"checks":[{"id":"crawler_blocked","level":"fail","url":"https://alpha.com/robots.txt","title":"OAI-SearchBot is blocked in robots.txt"},{"id":"title_ok","level":"pass","title":"Title length is good"}]}}}`)
		case "POST /v1/products/prd_a/page-suggestions":
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"status":"ok","data":{"id":"pgs_1","url":"https://alpha.com/pricing","lang":"en","suggestions":{"summary":"Lead with the price.",
				"title":{"text":"Alpha pricing — free for 3 sites","why":"alpha pricing at position 4"},"description":{"text":"","why":""},"h1":{"text":"","why":""},
				"sections":[{"heading":"What each plan includes","points":["Sites","API calls"],"why":"people compare plans"}],"faq":[{"question":"Is there a free plan?","answer":"Yes."}],"schema":[{"type":"Product","why":"prices"}]}}}`)
		case "GET /v1/annotations":
			io.WriteString(w, `{"status":"ok","data":[
				{"id":"ann_1","product_id":"prd_a","date":"2026-11-02","text":"Launched v2","created_by":"api"},
				{"id":"ann_2","date":"2026-11-12","text":"Google core update","created_by":"pat@example.com"}]}`)
		case "POST /v1/annotations":
			var in map[string]string
			_ = json.Unmarshal([]byte(f.bodies["POST /v1/annotations"]), &in)
			w.WriteHeader(201)
			fmt.Fprintf(w, `{"status":"ok","data":{"id":"ann_3","product_id":%q,"date":%q,"text":%q,"created_by":"api"}}`, in["product_id"], in["date"], in["text"])
		case "DELETE /v1/annotations/ann_1":
			io.WriteString(w, `{"status":"ok","data":{"deleted":true}}`)
		case "PATCH /v1/products/prd_a":
			var in struct {
				Name *string  `json:"name"`
				Tags []string `json:"tags"`
			}
			_ = json.Unmarshal([]byte(f.bodies["PATCH /v1/products/prd_a"]), &in)
			name := "Alpha"
			if in.Name != nil {
				name = *in.Name
			}
			tags, _ := json.Marshal(in.Tags)
			fmt.Fprintf(w, `{"status":"ok","data":{"id":"prd_a","name":%q,"domain":"alpha.com","tags":%s}}`, name, tags)
		case "GET /v1/products/import":
			if r.URL.Query().Get("gsc") != "con_g" {
				apiError(w, 400, "bad_request", "gsc must be a Search Console connection of this workspace")
				return
			}
			io.WriteString(w, `{"status":"ok","data":{"plan":"free","limit":3,"products":2,"sites":[
				{"external_id":"sc-domain:alpha.com","domain":"alpha.com","name":"Alpha","exists":true},
				{"external_id":"sc-domain:gamma.dev","domain":"gamma.dev","name":"Gamma","exists":false,"ga4":{"external_id":"properties/9","label":"Gamma Web","connection_id":"con_a"}},
				{"external_id":"https://www.delta.app/","domain":"delta.app","name":"Delta","exists":false}]}}`)
		case "POST /v1/products/import":
			var in struct {
				Sites []string `json:"sites"`
			}
			_ = json.Unmarshal([]byte(f.bodies["POST /v1/products/import"]), &in)
			if len(in.Sites) > 1 {
				apiError(w, 402, "plan_limit", "the free plan holds 3 products; upgrade the plan to add more")
				return
			}
			w.WriteHeader(202)
			io.WriteString(w, `{"status":"ok","data":{"created":[{"id":"prd_g","name":"Gamma","domain":"gamma.dev","ga4":true}]}}`)
		case "DELETE /v1/products/prd_b":
			io.WriteString(w, `{"status":"ok","data":{"deleted":true}}`)
		default:
			apiError(w, 404, "not_found", "not found")
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// oauth serves the authorization server endpoints. The authorize step
// approves at once, as a person on the consent page would. f.mu is held.
func (f *fakeAPI) oauth(w http.ResponseWriter, r *http.Request) bool {
	oauthErr := func(status int, code, desc string) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":%q,"error_description":%q}`, code, desc)
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /.well-known/oauth-authorization-server":
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":"%[1]s/oauth/authorize","token_endpoint":"%[1]s/oauth/token",
			"registration_endpoint":"%[1]s/oauth/register","revocation_endpoint":"%[1]s/oauth/revoke","code_challenge_methods_supported":["S256"]}`, f.URL)
	case "POST /oauth/register":
		var in struct {
			Name         string   `json:"client_name"`
			RedirectURIs []string `json:"redirect_uris"`
			AuthMethod   string   `json:"token_endpoint_auth_method"`
		}
		_ = json.Unmarshal([]byte(f.bodies["POST /oauth/register"]), &in)
		if in.Name != "Shipfolio CLI" || in.AuthMethod != "none" || len(in.RedirectURIs) != 1 || !strings.HasPrefix(in.RedirectURIs[0], "http://127.0.0.1:") {
			oauthErr(400, "invalid_client_metadata", "bad registration")
			return true
		}
		f.redirect = in.RedirectURIs[0]
		w.WriteHeader(201)
		io.WriteString(w, `{"client_id":"cli_1","token_endpoint_auth_method":"none"}`)
	case "GET /oauth/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != "cli_1" || q.Get("redirect_uri") != f.redirect {
			http.Error(w, "This sign-in link is not valid", 400)
			return true
		}
		if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("scope") != "metrics:read" || q.Get("state") == "" {
			http.Redirect(w, r, f.redirect+"?error=invalid_request&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
			return true
		}
		f.challenge = q.Get("code_challenge")
		state := q.Get("state")
		if f.tamperState {
			state = "forged"
		}
		http.Redirect(w, r, f.redirect+"?"+url.Values{"code": {"code_1"}, "state": {state}, "iss": {f.URL}}.Encode(), http.StatusFound)
	case "POST /oauth/token":
		_ = r.ParseForm()
		form := r.PostForm
		if form.Get("client_id") != "cli_1" {
			oauthErr(401, "invalid_client", "unknown client")
			return true
		}
		switch form.Get("grant_type") {
		case "authorization_code":
			if form.Get("code") != "code_1" || form.Get("redirect_uri") != f.redirect || client.Challenge(form.Get("code_verifier")) != f.challenge {
				oauthErr(400, "invalid_grant", "the code is unknown, used or expired")
				return true
			}
		case "refresh_token":
			if f.refresh == "" || form.Get("refresh_token") != f.refresh {
				oauthErr(400, "invalid_grant", "the refresh token is unknown, used, expired or revoked; sign in again")
				return true
			}
		default:
			oauthErr(400, "unsupported_grant_type", "no")
			return true
		}
		f.issued++
		f.access, f.refresh = fmt.Sprintf("sfo_%d", f.issued), fmt.Sprintf("sfr_%d", f.issued)
		fmt.Fprintf(w, `{"access_token":%q,"token_type":"Bearer","expires_in":3600,"refresh_token":%q,"scope":"metrics:read"}`, f.access, f.refresh)
	case "POST /oauth/revoke":
		_ = r.ParseForm()
		tok := r.PostForm.Get("token")
		f.revoked = append(f.revoked, tok)
		if tok == f.refresh || tok == f.access {
			f.access, f.refresh = "", ""
		}
		io.WriteString(w, `{}`)
	default:
		return false
	}
	return true
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
	if got := api.lastCall(); got != "GET /v1/overview?period=last_28_days" {
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
	last := api.lastCall()
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
	for _, c := range api.Calls() {
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
	for _, c := range api.Calls() {
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
	if r.code != 1 || !strings.Contains(r.stdout, `"not_found"`) || api.Calls()[0] != "GET /v1/nope?x=1" {
		t.Fatalf("%+v calls=%v", r, api.Calls())
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

// browserApp is an app whose "browser" follows the authorize URL the way a
// person's would: the fake server approves at once and redirects to the
// CLI's loopback callback.
func browserApp(t *testing.T, env map[string]string) *App {
	app := newApp(t, env, "", false)
	app.OpenBrowser = func(u string) error {
		resp, err := http.Get(u)
		if err != nil {
			t.Errorf("browser: %v", err)
			return err
		}
		defer resp.Body.Close()
		page, _ := io.ReadAll(resp.Body)
		if !strings.HasPrefix(resp.Request.URL.String(), "http://127.0.0.1:") {
			t.Errorf("browser ended at %s", resp.Request.URL)
		}
		if resp.StatusCode == 200 && !strings.Contains(string(page), "You can close this tab") {
			t.Errorf("callback page: %s", page)
		}
		return nil
	}
	return app
}

func savedConfig(t *testing.T, app *App) config {
	t.Helper()
	raw, err := os.ReadFile(app.Getenv("SHIPFOLIO_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBrowserLoginRefreshAndLogout(t *testing.T) {
	api := newFakeAPI(t)
	app := browserApp(t, nil)
	r := app.exec("login", "--browser", "--api-url", api.URL)
	if r.code != 0 || !strings.Contains(r.stdout, "Signed in to Acme") || !strings.Contains(r.stdout, "read data only") {
		t.Fatalf("login: %+v", r)
	}
	if !strings.Contains(r.stderr, api.URL+"/oauth/authorize?") || !strings.Contains(r.stderr, "code_challenge_method=S256") {
		t.Fatalf("the authorize URL was not printed: %s", r.stderr)
	}
	fi, err := os.Stat(app.Getenv("SHIPFOLIO_CONFIG"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config: %v %v", fi, err)
	}
	cfg := savedConfig(t, app)
	if cfg.APIKey != "" || cfg.OAuth == nil || cfg.OAuth.ClientID != "cli_1" || cfg.OAuth.AccessToken != "sfo_1" || cfg.OAuth.RefreshToken != "sfr_1" ||
		cfg.OAuth.TokenEndpoint != api.URL+"/oauth/token" || cfg.OAuth.Expiry.Before(time.Now().Add(50*time.Minute)) || cfg.APIURL != api.URL {
		t.Fatalf("saved %+v %+v", cfg, cfg.OAuth)
	}

	// The access token is sent as a bearer token.
	if r := app.exec("whoami"); r.code != 0 || !strings.Contains(r.stdout, "through the browser") || !strings.Contains(r.stdout, "metrics:read") {
		t.Fatalf("whoami: %+v", r)
	}

	// A write is refused for lack of scope, with a pointer to API keys.
	if r := app.exec("products", "rename", "alpha.com", "--name", "Alpha Two"); r.code != 1 || !strings.Contains(r.stderr, "insufficient_scope") || !strings.Contains(r.stderr, "API key") {
		t.Fatalf("write with an app token: %+v", r)
	}

	// The server stops accepting the access token: the CLI refreshes once
	// on the 401, retries, and saves the rotated refresh token.
	api.set(func(f *fakeAPI) { f.access = "sfo_elsewhere" })
	if r := app.exec("overview"); r.code != 0 || !strings.Contains(r.stdout, "12,345") {
		t.Fatalf("overview after a 401: %+v", r)
	}
	if cfg := savedConfig(t, app); cfg.OAuth.AccessToken != "sfo_2" || cfg.OAuth.RefreshToken != "sfr_2" {
		t.Fatalf("after refresh on 401: %+v", cfg.OAuth)
	}

	// An expired token is refreshed before the request is sent.
	app.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	n := len(api.Calls())
	if r := app.exec("whoami"); r.code != 0 {
		t.Fatalf("whoami after expiry: %+v", r)
	}
	calls := api.Calls()[n:]
	if len(calls) != 2 || calls[0] != "POST /oauth/token" || calls[1] != "GET /v1/whoami" {
		t.Fatalf("calls after expiry = %v", calls)
	}
	if cfg := savedConfig(t, app); cfg.OAuth.RefreshToken != "sfr_3" {
		t.Fatalf("after refresh on expiry: %+v", cfg.OAuth)
	}

	// logout revokes the grant on the server and forgets it.
	if r := app.exec("logout"); r.code != 0 || !strings.Contains(r.stdout, "ended the browser sign-in") {
		t.Fatalf("logout: %+v", r)
	}
	api.mu.Lock()
	revoked := append([]string(nil), api.revoked...)
	api.mu.Unlock()
	if len(revoked) != 1 || revoked[0] != "sfr_3" {
		t.Fatalf("revoked %v", revoked)
	}
	if cfg := savedConfig(t, app); cfg.OAuth != nil || cfg.APIURL != api.URL {
		t.Fatalf("after logout %+v", cfg)
	}
	if r := app.exec("whoami"); r.code != 1 || !strings.Contains(r.stderr, "no API key") {
		t.Fatalf("whoami after logout: %+v", r)
	}
}

func TestBrowserLoginExpiredRefreshAsksToSignInAgain(t *testing.T) {
	api := newFakeAPI(t)
	app := browserApp(t, nil)
	if r := app.exec("login", "--browser", "--api-url", api.URL); r.code != 0 {
		t.Fatalf("login: %+v", r)
	}
	// The grant was disconnected in the dashboard.
	api.set(func(f *fakeAPI) { f.access, f.refresh = "", "" })
	r := app.exec("whoami")
	if r.code != 1 || !strings.Contains(r.stderr, "login --browser") || strings.Contains(r.stderr, "sfr_") {
		t.Fatalf("%+v", r)
	}
}

func TestBrowserLoginChecksState(t *testing.T) {
	api := newFakeAPI(t)
	api.set(func(f *fakeAPI) { f.tamperState = true })
	app := browserApp(t, nil)
	r := app.exec("login", "--browser", "--api-url", api.URL)
	if r.code != 1 || !strings.Contains(r.stderr, "state differs") {
		t.Fatalf("%+v", r)
	}
	for _, c := range api.Calls() {
		if c == "POST /oauth/token" {
			t.Fatal("exchanged a code whose state did not match")
		}
	}
	if _, err := os.Stat(app.Getenv("SHIPFOLIO_CONFIG")); !os.IsNotExist(err) {
		t.Fatalf("config written: %v", err)
	}
}

func TestKeyLoginReplacesBrowserLogin(t *testing.T) {
	api := newFakeAPI(t)
	app := browserApp(t, nil)
	if r := app.exec("login", "--browser", "--api-url", api.URL); r.code != 0 {
		t.Fatalf("login: %+v", r)
	}
	app.Stdin = strings.NewReader("sf_live_good\n")
	if r := app.exec("login"); r.code != 0 {
		t.Fatalf("key login: %+v", r)
	}
	if cfg := savedConfig(t, app); cfg.OAuth != nil || cfg.APIKey != "sf_live_good" {
		t.Fatalf("saved %+v", cfg)
	}
	if r := app.exec("whoami"); r.code != 0 || !strings.Contains(r.stdout, "with an API key") {
		t.Fatalf("whoami: %+v", r)
	}
}

func TestOverviewShowsBingAndComparesYear(t *testing.T) {
	api := newFakeAPI(t)
	r := keyed(t, api).exec("overview", "--period", "1y", "--compare", "year")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := api.lastCall(); got != "GET /v1/overview?compare=year&period=last_365_days" {
		t.Fatalf("call = %q", got)
	}
	if !strings.Contains(r.stdout, "BING CLICKS") || !strings.Contains(r.stdout, "(a year earlier)") {
		t.Fatalf("overview:\n%s", r.stdout)
	}
	for _, line := range strings.Split(r.stdout, "\n") {
		f := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "alpha.com"):
			if len(f) != 12 || f[10] != "321" || f[11] != "+50.0%" {
				t.Errorf("alpha row = %q", line)
			}
		case strings.HasPrefix(line, "beta.io"):
			if len(f) != 12 || f[10] != none || f[11] != none {
				t.Errorf("beta row = %q", line)
			}
		}
	}
}

func TestBreakdownBing(t *testing.T) {
	api := newFakeAPI(t)
	r := keyed(t, api).exec("breakdown", "alpha.com", "bing_query")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"BING QUERY", "BING CLICKS", "BING CTR", "4.0%", "BING POSITION", "3.2"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("lacks %q:\n%s", want, r.stdout)
		}
	}
	if strings.Contains(r.stdout, "SESSIONS") {
		t.Errorf("empty column shown:\n%s", r.stdout)
	}
}

func TestMovers(t *testing.T) {
	api := newFakeAPI(t)
	app := keyed(t, api)
	r := app.exec("movers", "alpha.com", "query", "--compare", "year", "--limit", "5", "--period", "28d")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := api.lastCall(); got != "GET /v1/metrics/movers?compare=year&dimension=query&limit=5&period=last_28_days&product_id=prd_a" {
		t.Fatalf("call = %q", got)
	}
	for _, want := range []string{"Gainers", "Losers", "against 2025-10-24 to 2025-11-20 (a year earlier)", "1,200", "+200", "+20.0%", "-45", "-90.0%"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("lacks %q:\n%s", want, r.stdout)
		}
	}
	// No change from zero: a dash, never 0%.
	for _, line := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(line, "alpha new") && !strings.HasSuffix(strings.TrimSpace(line), none) {
			t.Errorf("new row = %q", line)
		}
	}
	r = app.exec("movers", "alpha.com", "query", "--csv")
	want := "direction,query,previous,current,delta,change\ngainer,alpha app,1000,1200,200,0.2\ngainer,alpha new,0,30,30,\nloser,alpha old,50,5,-45,-0.9\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("csv %q", r.stdout)
	}
	if r := app.exec("movers", "alpha.com", "query", "--limit", "51"); r.code != 2 {
		t.Fatalf("limit: %+v", r)
	}
}

func TestNotes(t *testing.T) {
	api := newFakeAPI(t)
	app := keyed(t, api)
	app.Now = func() time.Time { return time.Date(2026, 11, 25, 9, 0, 0, 0, time.Local) }

	r := app.exec("notes", "--product", "alpha.com", "--start", "2026-11-01")
	if r.code != 0 || !strings.Contains(r.stdout, "Launched v2") || !strings.Contains(r.stdout, "all products") || !strings.Contains(r.stdout, "ann_1") {
		t.Fatalf("notes: %+v", r)
	}
	if got := api.lastCall(); got != "GET /v1/annotations?product_id=prd_a&start=2026-11-01" {
		t.Fatalf("call = %q", got)
	}

	r = app.exec("note", "add", "--product", "alpha.com", "today", "Launched", "v3")
	if r.code != 0 || !strings.Contains(r.stdout, "Added note ann_3 on 2026-11-25 for alpha.com") {
		t.Fatalf("note add: %+v", r)
	}
	if got := api.body("POST /v1/annotations"); got != `{"date":"2026-11-25","product_id":"prd_a","text":"Launched v3"}` {
		t.Fatalf("body = %s", got)
	}
	if r := app.exec("note", "add", "2026-11-24", "Pricing page"); r.code != 0 || !strings.Contains(r.stdout, "every product") {
		t.Fatalf("note add for all: %+v", r)
	}
	if got := api.body("POST /v1/annotations"); got != `{"date":"2026-11-24","text":"Pricing page"}` {
		t.Fatalf("body = %s", got)
	}
	if r := app.exec("note", "add", "11/24", "x"); r.code != 2 {
		t.Fatalf("bad date: %+v", r)
	}

	if r := app.exec("note", "rm", "ann_1"); r.code != 2 || !strings.Contains(r.stderr, "Launched v2") || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("rm without --yes: %+v", r)
	}
	for _, c := range api.Calls() {
		if strings.HasPrefix(c, "DELETE") {
			t.Fatal("deleted without confirmation")
		}
	}
	if r := app.exec("note", "rm", "ann_1", "--yes"); r.code != 0 || api.lastCall() != "DELETE /v1/annotations/ann_1" {
		t.Fatalf("rm --yes: %+v", r)
	}
}

func TestProductsRename(t *testing.T) {
	api := newFakeAPI(t)
	app := keyed(t, api)
	r := app.exec("products", "rename", "alpha.com", "--name", "Alpha Two", "--tags", "b2b, tools")
	if r.code != 0 || !strings.Contains(r.stdout, "Updated prd_a: Alpha Two, alpha.com, tags b2b, tools.") {
		t.Fatalf("%+v", r)
	}
	if got := api.body("PATCH /v1/products/prd_a"); got != `{"name":"Alpha Two","tags":["b2b","tools"]}` {
		t.Fatalf("body = %s", got)
	}
	// An empty --tags clears them; it is sent, not dropped.
	if r := app.exec("products", "rename", "Alpha", "--tags", ""); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if got := api.body("PATCH /v1/products/prd_a"); got != `{"tags":[]}` {
		t.Fatalf("body = %s", got)
	}
	if r := app.exec("products", "rename", "alpha.com"); r.code != 2 {
		t.Fatalf("no changes: %+v", r)
	}
}

func TestProductsImport(t *testing.T) {
	api := newFakeAPI(t)
	app := keyed(t, api)

	// Without --all or sites it only previews.
	r := app.exec("products", "import", "--gsc", "con_g", "--ga4", "con_a")
	if r.code != 0 {
		t.Fatalf("preview: %+v", r)
	}
	for _, want := range []string{"sc-domain:gamma.dev", "has a product", "Gamma Web (properties/9)", "no match", "free plan holds 3 products; the workspace has 2", "Nothing was created"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("preview lacks %q:\n%s", want, r.stdout)
		}
	}
	if got := api.lastCall(); got != "GET /v1/products/import?ga4=con_a&gsc=con_g" {
		t.Fatalf("call = %q", got)
	}

	// Creating asks first.
	if r := app.exec("products", "import", "--gsc", "con_g", "--all"); r.code != 2 || !strings.Contains(r.stderr, "--yes") {
		t.Fatalf("without --yes: %+v", r)
	}

	// One site, by domain; one that already exists is skipped.
	r = app.exec("products", "import", "--gsc", "con_g", "--ga4", "con_a", "gamma.dev", "alpha.com", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "Created 1 product(s)") || !strings.Contains(r.stdout, "prd_g") || !strings.Contains(r.stderr, "alpha.com already has a product") {
		t.Fatalf("import: %+v", r)
	}
	if got := api.body("POST /v1/products/import"); got != `{"ga4":"con_a","gsc":"con_g","sites":["sc-domain:gamma.dev"]}` {
		t.Fatalf("body = %s", got)
	}

	// --all picks every new site; the plan limit's message is shown.
	r = app.exec("products", "import", "--gsc", "con_g", "--all", "--yes")
	if r.code != 1 || !strings.Contains(r.stderr, "the free plan holds 3 products; upgrade the plan to add more") {
		t.Fatalf("plan limit: %+v", r)
	}
	if got := api.body("POST /v1/products/import"); got != `{"gsc":"con_g","sites":["sc-domain:gamma.dev","https://www.delta.app/"]}` {
		t.Fatalf("body = %s", got)
	}

	if r := app.exec("products", "import", "--gsc", "con_g", "nope.example", "--yes"); r.code != 1 || !strings.Contains(r.stderr, "no Search Console site") {
		t.Fatalf("unknown site: %+v", r)
	}
	if r := app.exec("products", "import", "--all"); r.code != 2 {
		t.Fatalf("no --gsc: %+v", r)
	}
}

func TestMCPMentionsOAuth(t *testing.T) {
	app := newApp(t, map[string]string{"SHIPFOLIO_API_URL": "https://api.example.test"}, "", false)
	r := app.exec("mcp")
	if r.code != 0 || !strings.Contains(r.stdout, "https://api.example.test/mcp") || !strings.Contains(r.stdout, "need only that URL") {
		t.Fatalf("%+v", r)
	}
}

func TestNewUsageErrors(t *testing.T) {
	app := newApp(t, nil, "", false)
	for _, args := range [][]string{
		{"overview", "--compare", "week"},
		{"movers", "alpha.com"},
		{"note", "add", "today"},
		{"notes", "--start", "yesterday"},
		{"login", "--browser", "extra"},
	} {
		if r := app.exec(args...); r.code != 2 {
			t.Errorf("%v: code %d, stderr %q", args, r.code, r.stderr)
		}
	}
}

func TestTasksAuditAndSuggest(t *testing.T) {
	api := newFakeAPI(t)
	app := keyed(t, api)

	r := app.exec("tasks", "--product", "alpha.com")
	if r.code != 0 || !strings.Contains(r.stdout, "tsk_1") || !strings.Contains(r.stdout, "OAI-SearchBot is blocked") || !strings.Contains(r.stdout, "tasks done ID") {
		t.Fatalf("tasks: %+v", r)
	}
	if got := api.lastCall(); got != "GET /v1/tasks?product_id=prd_a&status=open" {
		t.Fatalf("call = %q", got)
	}
	if r := app.exec("tasks", "--status", "later"); r.code != 2 {
		t.Fatalf("bad status: %+v", r)
	}
	r = app.exec("tasks", "--fix")
	if r.code != 0 || !strings.Contains(r.stdout, "How to fix it:\n  User-agent: OAI-SearchBot\n  Allow: /") {
		t.Fatalf("tasks --fix: %q", r.stdout)
	}
	if !strings.Contains(r.stdout, "done 2026-10-01 (clicks +50") {
		t.Fatalf("done task's outcome missing: %q", r.stdout)
	}
	r = app.exec("tasks", "show", "tsk_2")
	if r.code != 0 || !strings.Contains(r.stdout, "Outcome for the query: 100 clicks in the 28 days before, 150 in the 28 after") {
		t.Fatalf("tasks show: %q", r.stdout)
	}
	if r := app.exec("tasks", "show", "tsk_404"); r.code != 1 {
		t.Fatalf("unknown task: %+v", r)
	}
	r = app.exec("tasks", "done", "tsk_1")
	if r.code != 0 || !strings.Contains(r.stdout, "Done: OAI-SearchBot is blocked") {
		t.Fatalf("tasks done: %+v", r)
	}
	if got := api.body("PATCH /v1/tasks/tsk_1"); got != `{"status":"done"}` {
		t.Fatalf("body = %s", got)
	}
	r = app.exec("tasks", "add", "alpha.com", "Add", "an", "FAQ", "--url", "https://alpha.com/pricing")
	if r.code != 0 || !strings.Contains(r.stdout, "Added task tsk_9") {
		t.Fatalf("tasks add: %+v", r)
	}
	if got := api.body("POST /v1/tasks"); got != `{"product_id":"prd_a","source":"manual","title":"Add an FAQ","url":"https://alpha.com/pricing"}` {
		t.Fatalf("body = %s", got)
	}

	r = app.exec("audit", "alpha.com")
	if r.code != 0 || !strings.Contains(r.stdout, "score 57/100") || !strings.Contains(r.stdout, "Blocked in robots.txt: OAI-SearchBot (search)") || strings.Contains(r.stdout, "Title length is good") {
		t.Fatalf("audit: %q", r.stdout)
	}
	if r := app.exec("audit", "alpha.com", "--run", "--all"); r.code != 0 || !strings.Contains(r.stdout, "Title length is good") {
		t.Fatalf("audit --run --all: %+v", r)
	}
	if got := api.lastCall(); got != "POST /v1/products/prd_a/site-audit" {
		t.Fatalf("call = %q", got)
	}

	r = app.exec("suggest", "alpha.com", "https://alpha.com/pricing")
	if r.code != 0 || !strings.Contains(r.stdout, "Title: Alpha pricing — free for 3 sites") || !strings.Contains(r.stdout, "Add: What each plan includes") || strings.Contains(r.stdout, "Description:") {
		t.Fatalf("suggest: %q", r.stdout)
	}
	if got := api.body("POST /v1/products/prd_a/page-suggestions"); got != `{"lang":"en","url":"https://alpha.com/pricing"}` {
		t.Fatalf("body = %s", got)
	}
	if r := app.exec("suggest", "alpha.com", "https://alpha.com/x", "--lang", "fr"); r.code != 2 {
		t.Fatalf("bad lang: %+v", r)
	}
}
