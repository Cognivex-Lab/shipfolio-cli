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
}

const usageText = `shipfolio reads your Shipfolio workspace from the terminal.

Usage:
  shipfolio <command> [arguments] [flags]

Account:
  login                      Save an API key (read from stdin or a prompt)
  logout                     Forget the saved key
  whoami                     Show the workspace and scopes of the key

Data:
  overview                   Every product's totals against the period before
  metrics PRODUCT...         Daily, weekly or monthly series
  breakdown PRODUCT DIM      Top queries, pages, countries, devices, channels or landing pages
  alerts                     Traffic-drop alerts, newest first

Products and sources:
  products                   List products and their data sources
  products add NAME DOMAIN   Add a product
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
		err = a.logout(rest)
	case "whoami":
		err = a.whoami(ctx, rest)
	case "overview":
		err = a.overview(ctx, rest)
	case "metrics":
		err = a.metrics(ctx, rest)
	case "breakdown":
		err = a.breakdown(ctx, rest)
	case "alerts":
		err = a.alerts(ctx, rest)
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
			fmt.Fprintln(a.Stderr, "Check the key, or run 'shipfolio login' with a new one.")
		case "insufficient_scope":
			fmt.Fprintln(a.Stderr, "This key lacks the scope for that. An admin can create a key with more scopes in the dashboard.")
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

// client returns a client with the key from the environment or the config.
func (a *App) client(c *common) (*client.Client, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return nil, err
	}
	key := a.Getenv("SHIPFOLIO_API_KEY")
	if key == "" {
		key = cfg.APIKey
	}
	if key == "" {
		return nil, errors.New("no API key: run 'shipfolio login' or set SHIPFOLIO_API_KEY")
	}
	return a.newClient(c, key, cfg), nil
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
