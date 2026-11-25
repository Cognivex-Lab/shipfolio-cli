// Package cli implements the shipfolio command.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Cognivex-Lab/shipfolio-cli/internal/client"
)

// DefaultAPIURL is the hosted Shipfolio API.
const DefaultAPIURL = "https://api.shipfolio.ai"

// Version is set at build time with -ldflags "-X …/internal/cli.Version=v1.2.3".
var Version = "dev"

// App holds everything a command touches outside the process, so tests can
// replace it.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Getenv func(string) string
	HTTP   *http.Client
	// Interactive reports whether stdin is a terminal a person types into.
	Interactive func() bool
	// ReadSecret prompts for a value without echoing it.
	ReadSecret func(prompt string) (string, error)
	// OpenBrowser opens a URL in the person's browser. It may be nil; the
	// URL is printed as well.
	OpenBrowser func(url string) error
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// oauth is set once a command authenticates with a browser sign-in,
	// so errors can suggest what fits that kind of login.
	oauth bool
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

const usageText = `shipfolio reads your Shipfolio workspace from the terminal.

Usage:
  shipfolio <command> [arguments] [flags]

Account:
  login                      Save an API key (read from stdin or a prompt)
  login --browser            Sign in through the browser instead (read-only)
  logout                     Forget the saved key or sign-in
  whoami                     Show the workspace and scopes of the key

Data:
  overview                   Every product's totals against the period before
  metrics PRODUCT...         Daily, weekly or monthly series
  breakdown PRODUCT DIM      Top queries, pages, countries, devices, channels,
                             landing pages, Bing queries or Bing pages
  movers PRODUCT DIM         What gained and lost the most against the period before
  alerts                     Traffic-drop alerts, newest first
  notes                      Annotations on the charts
  note add DATE TEXT         Annotate a day (DATE may be today)
  note rm ID                 Delete an annotation

Products and sources:
  products                   List products and their data sources
  products add NAME DOMAIN   Add a product
  products rename PRODUCT    Change a product's name, domain or tags
  products import --gsc CONN Create products from a Google account's Search Console sites
  products rm PRODUCT        Delete a product with its history
  sources list PRODUCT       List a product's data sources
  sources add PRODUCT        Connect a Search Console site or GA4 property
  sources rm PRODUCT SOURCE  Disconnect a source and delete its data
  sync PRODUCT               Re-import the last few days now
  google connections         Google accounts connected to the workspace
  google resources CONN      Sites and properties a connection can read

Other:
  api PATH                   Send a raw request and print the JSON response
  mcp                        Show how to connect an AI agent over MCP
  version                    Print the version

PRODUCT is a product id, domain or name. Every command takes --json for the
raw API response and --api-url to use another server.

Environment:
  SHIPFOLIO_API_KEY   API key; overrides the saved one
  SHIPFOLIO_API_URL   API base URL (default ` + DefaultAPIURL + `)
  SHIPFOLIO_CONFIG    Config file path

Create an API key in the dashboard under Connections & API.
`

// Run executes one command line and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.Stdout, usageText)
		return 0
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "help", "-h", "--help":
		fmt.Fprint(a.Stdout, usageText)
		return 0
	case "version", "-v", "--version":
		fmt.Fprintln(a.Stdout, "shipfolio", Version)
		return 0
	case "login":
		err = a.login(ctx, rest)
	case "logout":
		err = a.logout(ctx, rest)
	case "whoami":
		err = a.whoami(ctx, rest)
	case "overview":
		err = a.overview(ctx, rest)
	case "metrics":
		err = a.metrics(ctx, rest)
	case "breakdown":
		err = a.breakdown(ctx, rest)
	case "movers":
		err = a.movers(ctx, rest)
	case "alerts":
		err = a.alerts(ctx, rest)
	case "notes", "note", "annotations":
		err = a.notes(ctx, rest)
	case "products", "product":
		err = a.products(ctx, rest)
	case "sources", "source":
		err = a.sources(ctx, rest)
	case "sync":
		err = a.sync(ctx, rest)
	case "google":
		err = a.google(ctx, rest)
	case "api":
		err = a.api(ctx, rest)
	case "mcp":
		err = a.mcp(rest)
	default:
		err = usagef("unknown command %q", cmd)
	}
	return a.report(err)
}

// usageError is a mistake in the command line.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return usageError{fmt.Sprintf(format, args...)}
}

// errQuiet ends a command whose output already said what went wrong.
var errQuiet = errors.New("quiet")

func (a *App) report(err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if errors.Is(err, errQuiet) {
		return 1
	}
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(a.Stderr, "shipfolio: %s\nRun 'shipfolio help' for usage.\n", ue.msg)
		return 2
	}
	fmt.Fprintf(a.Stderr, "shipfolio: %s\n", err)
	var ae *client.Error
	if errors.As(err, &ae) {
		switch ae.Code {
		case "unauthorized":
			if a.oauth {
				fmt.Fprintln(a.Stderr, "Sign in again with 'shipfolio login --browser'.")
			} else {
				fmt.Fprintln(a.Stderr, "Check the key, or run 'shipfolio login' with a new one.")
			}
		case "insufficient_scope":
			if a.oauth {
				fmt.Fprintln(a.Stderr, "A browser sign-in can only read (metrics:read). To change anything, create an API key with the sync:write scope in the dashboard under Connections & API, then run 'shipfolio login' and paste it.")
			} else {
				fmt.Fprintln(a.Stderr, "This key lacks the scope for that. An admin can create a key with more scopes in the dashboard.")
			}
		case "reconnect_required":
			fmt.Fprintln(a.Stderr, "Reconnect the Google account in the dashboard, then try again.")
		}
	}
	return 1
}

// common holds the flags every command takes.
type common struct {
	json   bool
	apiURL string
}

func newFlags(name, usage string, stderr io.Writer) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &common{}
	fs.BoolVar(&c.json, "json", false, "print the raw API response")
	fs.StringVar(&c.apiURL, "api-url", "", "API base URL")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: shipfolio %s\n\nFlags:\n", usage)
		fs.PrintDefaults()
	}
	return fs, c
}

// parse reads flags anywhere among the arguments, so both
// "metrics example.com --period 7d" and "metrics --period 7d example.com"
// work, and returns the positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, usageError{err.Error()}
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

type config struct {
	APIURL string `json:"api_url,omitempty"`
	APIKey string `json:"api_key,omitempty"`
	// OAuth holds a browser sign-in. At most one of APIKey and OAuth is set.
	OAuth *oauthLogin `json:"oauth,omitempty"`
}

// oauthLogin is a browser sign-in: tokens from the API's OAuth server.
type oauthLogin struct {
	ClientID       string    `json:"client_id"`
	AccessToken    string    `json:"access_token"`
	RefreshToken   string    `json:"refresh_token"`
	Expiry         time.Time `json:"expiry"`
	TokenEndpoint  string    `json:"token_endpoint"`
	RevokeEndpoint string    `json:"revocation_endpoint,omitempty"`
}

func (a *App) configPath() string {
	if p := a.Getenv("SHIPFOLIO_CONFIG"); p != "" {
		return p
	}
	dir := a.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := a.Getenv("HOME")
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "shipfolio", "config.json")
}

func (a *App) loadConfig() (config, error) {
	var cfg config
	raw, err := os.ReadFile(a.configPath())
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("read %s: %w", a.configPath(), err)
	}
	return cfg, nil
}

// saveConfig writes the file readable by its owner only: it holds a key.
func (a *App) saveConfig(cfg config) error {
	p := a.configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (a *App) apiURL(c *common, cfg config) string {
	u := c.apiURL
	if u == "" {
		u = a.Getenv("SHIPFOLIO_API_URL")
	}
	if u == "" {
		u = cfg.APIURL
	}
	if u == "" {
		u = DefaultAPIURL
	}
	return strings.TrimRight(u, "/")
}

func (a *App) newClient(c *common, key string, cfg config) *client.Client {
	return &client.Client{BaseURL: a.apiURL(c, cfg), Key: key, HTTP: a.HTTP, UserAgent: "shipfolio-cli/" + Version}
}

// client returns a client with the key from the environment or the
// config, or else the saved browser sign-in.
func (a *App) client(c *common) (*client.Client, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	key := a.Getenv("SHIPFOLIO_API_KEY")
	if key == "" {
		key = cfg.APIKey
	}
	if key == "" && cfg.OAuth != nil {
		cl := a.newClient(c, "", cfg)
		cl.Token = a.oauthToken(*cfg.OAuth)
		a.oauth = true
		return cl, nil
	}
	if key == "" {
		return nil, errors.New("no API key: run 'shipfolio login' (or 'shipfolio login --browser') or set SHIPFOLIO_API_KEY")
	}
	return a.newClient(c, key, cfg), nil
}

func (a *App) oauthClient() *client.OAuth {
	return &client.OAuth{HTTP: a.HTTP, UserAgent: "shipfolio-cli/" + Version}
}

// oauthToken returns the access token, refreshing it when it has expired
// or the server refused it. Refresh tokens rotate, so each new one is saved
// at once: the old one no longer works.
func (a *App) oauthToken(login oauthLogin) func(context.Context, bool) (string, error) {
	var mu sync.Mutex
	return func(ctx context.Context, renew bool) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if !renew && login.AccessToken != "" && a.now().Before(login.Expiry.Add(-time.Minute)) {
			return login.AccessToken, nil
		}
		if login.RefreshToken == "" || login.TokenEndpoint == "" {
			return "", errors.New("the browser sign-in has expired; run 'shipfolio login --browser' again")
		}
		t, err := a.oauthClient().Refresh(ctx, login.TokenEndpoint, login.ClientID, login.RefreshToken)
		if err != nil {
			var ae *client.Error
			if errors.As(err, &ae) && (ae.Code == "invalid_grant" || ae.Code == "invalid_client") {
				return "", errors.New("the browser sign-in has expired or was disconnected; run 'shipfolio login --browser' again")
			}
			return "", fmt.Errorf("refresh the browser sign-in: %w", err)
		}
		login.AccessToken = t.AccessToken
		login.Expiry = t.Expiry(a.now())
		if t.RefreshToken != "" {
			login.RefreshToken = t.RefreshToken
		}
		// Save into the file as it is now, so nothing else in it is lost.
		cfg, err := a.loadConfig()
		if err != nil {
			return "", err
		}
		if cfg.OAuth != nil && cfg.OAuth.ClientID == login.ClientID {
			saved := login
			cfg.OAuth = &saved
			if err := a.saveConfig(cfg); err != nil {
				return "", fmt.Errorf("save the refreshed sign-in: %w", err)
			}
		}
		return login.AccessToken, nil
	}
}

// printRaw prints a response body as indented JSON.
func (a *App) printRaw(raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		_, err := a.Stdout.Write(raw)
		return err
	}
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
