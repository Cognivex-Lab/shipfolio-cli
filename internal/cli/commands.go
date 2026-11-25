package cli

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Cognivex-Lab/shipfolio-cli/internal/client"
)

// fetch sends a request. With --json it prints the body and reports
// printed; otherwise it decodes the body into out.
func (a *App) fetch(ctx context.Context, cl *client.Client, c *common, method, path string, q url.Values, body, out any) (printed bool, err error) {
	raw, err := cl.Do(ctx, method, path, q, body)
	if err != nil {
		return false, err
	}
	if c.json {
		return true, a.printRaw(raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return false, fmt.Errorf("unexpected response from %s: %w", path, err)
	}
	return false, nil
}

func (a *App) login(ctx context.Context, args []string) error {
	fs, c := newFlags("login", "login [--api-url URL] < key.txt\n       shipfolio login --browser [--api-url URL]", a.Stderr)
	browser := fs.Bool("browser", false, "sign in through the browser instead of pasting a key (read-only)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("login reads the key from a prompt or stdin, not from arguments, so it stays out of your shell history")
	}
	if *browser {
		cfg, err := a.loadConfig()
		if err != nil {
			return err
		}
		if c.apiURL != "" {
			cfg.APIURL = strings.TrimRight(c.apiURL, "/")
		}
		return a.loginBrowser(ctx, c, cfg)
	}
	var key string
	if a.Interactive() {
		key, err = a.ReadSecret("API key: ")
	} else {
		var b []byte
		b, err = io.ReadAll(io.LimitReader(a.Stdin, 4096))
		key = string(b)
	}
	if err != nil {
		return err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return usagef("no API key given")
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if c.apiURL != "" {
		cfg.APIURL = strings.TrimRight(c.apiURL, "/")
	}
	var who client.Envelope[client.Whoami]
	if err := a.newClient(c, key, cfg).Call(ctx, "GET", "/v1/whoami", nil, nil, &who); err != nil {
		return err
	}
	if cfg.OAuth != nil {
		a.revokeSaved(ctx, cfg.OAuth)
		cfg.OAuth = nil
	}
	cfg.APIKey = key
	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Signed in to %s (%s plan). The key is saved in %s.\n", who.Data.WorkspaceName, who.Data.Plan, a.configPath())
	if a.Getenv("SHIPFOLIO_API_KEY") != "" {
		fmt.Fprintln(a.Stderr, "SHIPFOLIO_API_KEY is set and is used instead of the saved key.")
	}
	return nil
}

func (a *App) logout(ctx context.Context, args []string) error {
	fs, _ := newFlags("logout", "logout", a.Stderr)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	if cfg.APIKey == "" && cfg.OAuth == nil {
		fmt.Fprintln(a.Stdout, "No key was saved.")
		return nil
	}
	login, hadKey := cfg.OAuth, cfg.APIKey != ""
	cfg.APIKey, cfg.OAuth = "", nil
	if err := a.saveConfig(cfg); err != nil {
		return err
	}
	if hadKey {
		fmt.Fprintln(a.Stdout, "Removed the saved key. It still works until you revoke it in the dashboard.")
	}
	if login != nil {
		if a.revokeSaved(ctx, login) {
			fmt.Fprintln(a.Stdout, "Signed out and ended the browser sign-in.")
		} else {
			fmt.Fprintln(a.Stdout, "Removed the saved browser sign-in, but could not end it on the server. Disconnect it in the dashboard under Account, Connected apps.")
		}
	}
	return nil
}

func (a *App) whoami(ctx context.Context, args []string) error {
	fs, c := newFlags("whoami", "whoami", a.Stderr)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	var env client.Envelope[client.Whoami]
	if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/whoami", nil, nil, &env); printed || err != nil {
		return err
	}
	w := env.Data
	tw := newTable(a.Stdout)
	row(tw, "Workspace", w.WorkspaceName)
	row(tw, "ID", w.WorkspaceID)
	row(tw, "Plan", w.Plan)
	row(tw, "Scopes", strings.Join(w.Scopes, ", "))
	if a.oauth {
		row(tw, "Signed in", "through the browser (read-only)")
	} else {
		row(tw, "Signed in", "with an API key")
	}
	row(tw, "API", cl.BaseURL)
	return tw.Flush()
}

// periodValue accepts 7d, 28d, 90d and 365d (or 1y) as well as the API's
// own names.
func periodValue(s string) (string, error) {
	switch strings.ToLower(s) {
	case "":
		return "", nil
	case "7", "7d", "last_7_days":
		return "last_7_days", nil
	case "28", "28d", "last_28_days":
		return "last_28_days", nil
	case "90", "90d", "last_90_days":
		return "last_90_days", nil
	case "365", "365d", "1y", "12m", "last_365_days":
		return "last_365_days", nil
	}
	return "", usagef("--period must be 7d, 28d, 90d or 365d")
}

// compareValue checks --compare: previous (the days just before) or year
// (the same days a year earlier).
func compareValue(s string) (string, error) {
	switch strings.ToLower(s) {
	case "":
		return "", nil
	case "previous", "prev":
		return "previous", nil
	case "year", "yoy", "last_year":
		return "year", nil
	}
	return "", usagef("--compare must be previous or year")
}

// rangeQuery adds --period or --start/--end to a query.
func rangeQuery(q url.Values, period, start, end string) error {
	if (start == "") != (end == "") {
		return usagef("give --start and --end together")
	}
	if start != "" {
		if period != "" {
			return usagef("give either --period or --start and --end")
		}
		q.Set("start", start)
		q.Set("end", end)
		return nil
	}
	p, err := periodValue(period)
	if err != nil {
		return err
	}
	if p != "" {
		q.Set("period", p)
	}
	return nil
}

func (a *App) overview(ctx context.Context, args []string) error {
	fs, c := newFlags("overview", "overview [--period 7d|28d|90d|365d] [--compare previous|year]", a.Stderr)
	period := fs.String("period", "", "7d, 28d (default), 90d or 365d")
	compare := fs.String("compare", "", "previous (default: the days just before) or year (the same days a year earlier)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	q := url.Values{}
	if err := rangeQuery(q, *period, "", ""); err != nil {
		return err
	}
	cmp, err := compareValue(*compare)
	if err != nil {
		return err
	}
	if cmp != "" {
		q.Set("compare", cmp)
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	var resp client.OverviewResponse
	if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/overview", q, nil, &resp); printed || err != nil {
		return err
	}
	o := resp.Data
	if len(o.Products) == 0 {
		fmt.Fprintln(a.Stdout, "No products yet. Add one with 'shipfolio products add NAME DOMAIN'.")
		return nil
	}
	if o.Period.Start != "" {
		fmt.Fprintf(a.Stdout, "%s to %s (%d days), against %s\n", o.Period.Start, o.Period.End, o.Period.Days, againstText(o.Period))
	}
	if !o.HasData {
		fmt.Fprintln(a.Stdout, "No data yet. Connect Search Console or GA4 to a product in the dashboard.")
	}
	fmt.Fprintln(a.Stdout)
	tw := newTable(a.Stdout)
	row(tw, "PRODUCT", "CLICKS", "CHANGE", "IMPRESSIONS", "CHANGE", "CTR", "POSITION", "SESSIONS", "CHANGE", "KEY EVENTS", "BING CLICKS", "CHANGE")
	for _, p := range o.Products {
		cur := p.Current
		row(tw, productLabel(p.Name, p.Domain),
			fmtMetric("clicks", cur.Clicks), fmtChange(p.Change["clicks"]),
			fmtMetric("impressions", cur.Impressions), fmtChange(p.Change["impressions"]),
			fmtMetric("ctr", cur.CTR), fmtMetric("avg_position", cur.AvgPosition),
			fmtMetric("sessions", cur.Sessions), fmtChange(p.Change["sessions"]),
			fmtMetric("key_events", cur.KeyEvents),
			fmtMetric("bing_clicks", cur.BingClicks), fmtChange(p.Change["bing_clicks"]))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if resp.AsOf != nil {
		fmt.Fprintf(a.Stdout, "\nData through %s. %s means Google reported nothing for it.\n", *resp.AsOf, none)
	}
	return nil
}

func (a *App) metrics(ctx context.Context, args []string) error {
	fs, c := newFlags("metrics", "metrics PRODUCT... [--metrics clicks,sessions] [--granularity day|week|month] [--period 28d | --start DATE --end DATE] [--csv]", a.Stderr)
	metrics := fs.String("metrics", "clicks,impressions,sessions", "comma-separated metric names")
	gran := fs.String("granularity", "day", "day, week or month")
	period := fs.String("period", "", "7d, 28d (default), 90d or 365d")
	start := fs.String("start", "", "first day, YYYY-MM-DD")
	end := fs.String("end", "", "last day, YYYY-MM-DD, inclusive")
	asCSV := fs.Bool("csv", false, "print CSV")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef("metrics needs at least one product")
	}
	switch *gran {
	case "day", "week", "month":
	default:
		return usagef("--granularity must be day, week or month")
	}
	names := splitList(*metrics)
	if len(names) == 0 {
		return usagef("--metrics is empty")
	}
	q := url.Values{"metrics": {strings.Join(names, ",")}, "granularity": {*gran}}
	if err := rangeQuery(q, *period, *start, *end); err != nil {
		return err
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	products, err := a.resolve(ctx, cl, pos)
	if err != nil {
		return err
	}
	ids := make([]string, len(products))
	byID := map[string]client.Product{}
	for i, p := range products {
		ids[i] = p.ID
		byID[p.ID] = p
	}
	q.Set("product_ids", strings.Join(ids, ","))
	var resp client.MetricsResponse
	if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/metrics", q, nil, &resp); printed || err != nil {
		return err
	}
	if *asCSV {
		w := csv.NewWriter(a.Stdout)
		_ = w.Write(append(append([]string{"product_id", "date"}, names...), "is_final"))
		for _, s := range resp.Data {
			for _, pt := range s.Points {
				rec := []string{s.ProductID, pt.Date}
				for _, n := range names {
					rec = append(rec, csvValue(pt.Values[n]))
				}
				_ = w.Write(append(rec, strconv.FormatBool(pt.IsFinal)))
			}
		}
		w.Flush()
		return w.Error()
	}
	pending := false
	for i, s := range resp.Data {
		if i > 0 {
			fmt.Fprintln(a.Stdout)
		}
		p := byID[s.ProductID]
		fmt.Fprintf(a.Stdout, "%s, by %s, %s to %s\n", productLabel(p.Name, p.Domain), *gran, resp.Start, resp.End)
		tw := newTable(a.Stdout)
		head := []string{"DATE"}
		for _, n := range names {
			head = append(head, metricTitle(n))
		}
		row(tw, head...)
		for _, pt := range s.Points {
			date := pt.Date
			if !pt.IsFinal {
				date += "*"
				pending = true
			}
			cells := []string{date}
			for _, n := range names {
				cells = append(cells, fmtMetric(n, pt.Values[n]))
			}
			row(tw, cells...)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if pending {
		fmt.Fprintln(a.Stdout, "\n* Google may still revise these numbers.")
	}
	return nil
}

func (a *App) breakdown(ctx context.Context, args []string) error {
	fs, c := newFlags("breakdown", "breakdown PRODUCT query|page|country|device|channel|landing_page|bing_query|bing_page [--metric clicks] [--limit 25] [--period 28d | --start DATE --end DATE] [--csv]", a.Stderr)
	metric := fs.String("metric", "", "sort by this metric (default clicks, sessions for GA4 dimensions, bing_clicks for Bing)")
	limit := fs.Int("limit", 25, "rows to return, 1 to 1000")
	period := fs.String("period", "", "7d, 28d (default), 90d or 365d")
	start := fs.String("start", "", "first day, YYYY-MM-DD")
	end := fs.String("end", "", "last day, YYYY-MM-DD, inclusive")
	asCSV := fs.Bool("csv", false, "print CSV")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef("breakdown needs a product and a dimension (query, page, country, device, channel, landing_page, bing_query or bing_page)")
	}
	if *limit < 1 || *limit > 1000 {
		return usagef("--limit must be between 1 and 1000")
	}
	q := url.Values{"dimension": {pos[1]}, "limit": {strconv.Itoa(*limit)}}
	if *metric != "" {
		q.Set("metric", *metric)
	}
	if err := rangeQuery(q, *period, *start, *end); err != nil {
		return err
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	products, err := a.resolve(ctx, cl, pos[:1])
	if err != nil {
		return err
	}
	q.Set("product_id", products[0].ID)
	var resp client.Envelope[client.Breakdown]
	if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/metrics/breakdown", q, nil, &resp); printed || err != nil {
		return err
	}
	b := resp.Data
	// Show only the metrics this dimension has: a column that is null in
	// every row would only say the same thing many times.
	seen := map[string]bool{}
	values := make([]map[string]*float64, len(b.Rows))
	labels := make([]string, len(b.Rows))
	for i, r := range b.Rows {
		values[i] = map[string]*float64{}
		for k, raw := range r {
			if k == "value" {
				_ = json.Unmarshal(raw, &labels[i])
				continue
			}
			var v *float64
			if json.Unmarshal(raw, &v) != nil {
				continue
			}
			values[i][k] = v
			if v != nil {
				seen[k] = true
			}
		}
	}
	var cols []string
	for k := range seen {
		cols = append(cols, k)
	}
	cols = sortMetrics(cols)
	if *asCSV {
		w := csv.NewWriter(a.Stdout)
		_ = w.Write(append([]string{b.Dimension}, cols...))
		for i := range b.Rows {
			rec := []string{labels[i]}
			for _, k := range cols {
				rec = append(rec, csvValue(values[i][k]))
			}
			_ = w.Write(rec)
		}
		w.Flush()
		return w.Error()
	}
	p := products[0]
	fmt.Fprintf(a.Stdout, "%s, top %s by %s, %s to %s\n\n", productLabel(p.Name, p.Domain), strings.ReplaceAll(b.Dimension, "_", " "), strings.ReplaceAll(b.SortedBy, "_", " "), b.Start, b.End)
	if len(b.Rows) == 0 {
		fmt.Fprintln(a.Stdout, "No rows for this range.")
	} else {
		tw := newTable(a.Stdout)
		head := []string{strings.ToUpper(strings.ReplaceAll(b.Dimension, "_", " "))}
		for _, k := range cols {
			head = append(head, metricTitle(k))
		}
		row(tw, head...)
		for i := range b.Rows {
			cells := []string{labels[i]}
			for _, k := range cols {
				cells = append(cells, fmtMetric(k, values[i][k]))
			}
			row(tw, cells...)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if b.Note != "" {
		fmt.Fprintf(a.Stdout, "\n%s\n", b.Note)
	}
	return nil
}

func (a *App) alerts(ctx context.Context, args []string) error {
	fs, c := newFlags("alerts", "alerts [--product PRODUCT]", a.Stderr)
	product := fs.String("product", "", "only this product")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	all, err := a.listProducts(ctx, cl)
	if err != nil {
		return err
	}
	q := url.Values{}
	if *product != "" {
		p, err := match(all, *product)
		if err != nil {
			return err
		}
		q.Set("product_id", p.ID)
	}
	var resp client.Envelope[[]client.Alert]
	if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/alerts/events", q, nil, &resp); printed || err != nil {
		return err
	}
	if len(resp.Data) == 0 {
		fmt.Fprintln(a.Stdout, "No alerts.")
		return nil
	}
	names := map[string]string{}
	for _, p := range all {
		names[p.ID] = productLabel(p.Name, p.Domain)
	}
	tw := newTable(a.Stdout)
	row(tw, "DATE", "PRODUCT", "ALERT")
	for _, e := range resp.Data {
		name := names[e.ProductID]
		if name == "" {
			name = e.ProductID
		}
		row(tw, e.Date, name, e.Message)
	}
	return tw.Flush()
}

func (a *App) products(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "list", "ls":
		return a.productsList(ctx, args)
	case "add":
		return a.productsAdd(ctx, args)
	case "rename", "edit", "update":
		return a.productsRename(ctx, args)
	case "import":
		return a.productsImport(ctx, args)
	case "rm", "delete", "remove":
		return a.productsRemove(ctx, args)
	}
	return usagef("unknown products command %q (list, add, rename, import or rm)", sub)
}

func (a *App) productsList(ctx context.Context, args []string) error {
	fs, c := newFlags("products", "products [list]", a.Stderr)
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) > 0 {
		return usagef("products list takes no arguments")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	var resp client.Envelope[[]client.Product]
	if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/products", nil, nil, &resp); printed || err != nil {
		return err
	}
	if len(resp.Data) == 0 {
		fmt.Fprintln(a.Stdout, "No products yet. Add one with 'shipfolio products add NAME DOMAIN'.")
		return nil
	}
	tw := newTable(a.Stdout)
	row(tw, "ID", "NAME", "DOMAIN", "SOURCES")
	for _, p := range resp.Data {
		var src []string
		for _, s := range p.Sources {
			label := s.Type
			if s.Status != "" && s.Status != "ok" {
				label += " (" + strings.ReplaceAll(s.Status, "_", " ") + ")"
			}
			src = append(src, label)
		}
		sources := strings.Join(src, ", ")
		if sources == "" {
			sources = "none"
		}
		row(tw, p.ID, p.Name, p.Domain, sources)
	}
	return tw.Flush()
}

func (a *App) productsAdd(ctx context.Context, args []string) error {
	fs, c := newFlags("products add", "products add NAME DOMAIN [--tags a,b]", a.Stderr)
	tags := fs.String("tags", "", "comma-separated tags")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef("products add needs a name and a domain, e.g. products add Acme acme.com")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	body := map[string]any{"name": pos[0], "domain": pos[1]}
	if t := splitList(*tags); len(t) > 0 {
		body["tags"] = t
	}
	var resp client.Envelope[client.Product]
	if printed, err := a.fetch(ctx, cl, c, "POST", "/v1/products", nil, body, &resp); printed || err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Added %s (%s). Connect Search Console or GA4 in the dashboard, or with 'shipfolio sources add'.\n", resp.Data.Name, resp.Data.ID)
	return nil
}

func (a *App) productsRemove(ctx context.Context, args []string) error {
	fs, c := newFlags("products rm", "products rm PRODUCT [--yes]", a.Stderr)
	yes := fs.Bool("yes", false, "delete without asking")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("products rm needs one product")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	ps, err := a.resolve(ctx, cl, pos)
	if err != nil {
		return err
	}
	p := ps[0]
	if err := a.confirm(*yes, fmt.Sprintf("Delete %s with its sources, history and alerts?", productLabel(p.Name, p.Domain))); err != nil {
		return err
	}
	var resp client.Envelope[map[string]any]
	if printed, err := a.fetch(ctx, cl, c, "DELETE", "/v1/products/"+url.PathEscape(p.ID), nil, nil, &resp); printed || err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Deleted %s.\n", productLabel(p.Name, p.Domain))
	return nil
}

func (a *App) sources(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("sources needs list, add or rm")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "list", "ls":
		fs, c := newFlags("sources list", "sources list PRODUCT", a.Stderr)
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef("sources list needs one product")
		}
		cl, err := a.client(c)
		if err != nil {
			return err
		}
		all, err := a.listProducts(ctx, cl)
		if err != nil {
			return err
		}
		p, err := match(all, pos[0])
		if err != nil {
			return err
		}
		if c.json {
			enc := json.NewEncoder(a.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(p.Sources)
		}
		if len(p.Sources) == 0 {
			fmt.Fprintf(a.Stdout, "%s has no data sources yet.\n", productLabel(p.Name, p.Domain))
			return nil
		}
		tw := newTable(a.Stdout)
		row(tw, "ID", "TYPE", "SITE OR PROPERTY", "STATUS", "LAST SYNCED")
		for _, s := range p.Sources {
			last := s.LastSyncedAt
			if last == "" {
				last = "never"
			}
			row(tw, s.ID, s.Type, s.ExternalID, strings.ReplaceAll(s.Status, "_", " "), last)
		}
		return tw.Flush()

	case "add":
		fs, c := newFlags("sources add", "sources add PRODUCT --connection CONN --external-id sc-domain:example.com|properties/123", a.Stderr)
		conn := fs.String("connection", "", "Google connection id (see 'shipfolio google connections')")
		ext := fs.String("external-id", "", "site or property (see 'shipfolio google resources CONN')")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 || *conn == "" || *ext == "" {
			return usagef("sources add needs a product, --connection and --external-id")
		}
		cl, err := a.client(c)
		if err != nil {
			return err
		}
		ps, err := a.resolve(ctx, cl, pos)
		if err != nil {
			return err
		}
		var resp client.Envelope[struct {
			SourceID string `json:"source_id"`
		}]
		body := map[string]string{"connection_id": *conn, "external_id": *ext}
		if printed, err := a.fetch(ctx, cl, c, "POST", "/v1/products/"+url.PathEscape(ps[0].ID)+"/sources", nil, body, &resp); printed || err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "Connected %s to %s (source %s). The import has started.\n", *ext, productLabel(ps[0].Name, ps[0].Domain), resp.Data.SourceID)
		return nil

	case "rm", "delete", "remove":
		fs, c := newFlags("sources rm", "sources rm PRODUCT SOURCE [--yes]", a.Stderr)
		yes := fs.Bool("yes", false, "disconnect without asking")
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 2 {
			return usagef("sources rm needs a product and a source id")
		}
		cl, err := a.client(c)
		if err != nil {
			return err
		}
		ps, err := a.resolve(ctx, cl, pos[:1])
		if err != nil {
			return err
		}
		p := ps[0]
		if err := a.confirm(*yes, fmt.Sprintf("Disconnect %s from %s and delete the data it imported?", pos[1], productLabel(p.Name, p.Domain))); err != nil {
			return err
		}
		var resp client.Envelope[map[string]any]
		path := "/v1/products/" + url.PathEscape(p.ID) + "/sources/" + url.PathEscape(pos[1])
		if printed, err := a.fetch(ctx, cl, c, "DELETE", path, nil, nil, &resp); printed || err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "Disconnected %s.\n", pos[1])
		return nil
	}
	return usagef("unknown sources command %q (list, add or rm)", sub)
}

func (a *App) sync(ctx context.Context, args []string) error {
	fs, c := newFlags("sync", "sync PRODUCT", a.Stderr)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("sync needs one product")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	ps, err := a.resolve(ctx, cl, pos)
	if err != nil {
		return err
	}
	var resp client.Envelope[struct {
		QueuedSources int `json:"queued_sources"`
	}]
	if printed, err := a.fetch(ctx, cl, c, "POST", "/v1/products/"+url.PathEscape(ps[0].ID)+"/sync", nil, nil, &resp); printed || err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Re-importing %d source(s) for %s.\n", resp.Data.QueuedSources, productLabel(ps[0].Name, ps[0].Domain))
	return nil
}

func (a *App) google(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef("google needs connections or resources")
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "connections":
		fs, c := newFlags("google connections", "google connections", a.Stderr)
		if _, err := parse(fs, args); err != nil {
			return err
		}
		cl, err := a.client(c)
		if err != nil {
			return err
		}
		var resp client.Envelope[[]client.Connection]
		if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/google/connections", nil, nil, &resp); printed || err != nil {
			return err
		}
		if len(resp.Data) == 0 {
			fmt.Fprintln(a.Stdout, "No Google accounts are connected. Connect one in the dashboard under Connections & API.")
			return nil
		}
		tw := newTable(a.Stdout)
		row(tw, "ID", "PROVIDER", "ACCOUNT", "CONNECTED")
		for _, x := range resp.Data {
			row(tw, x.ID, x.Provider, x.Email, x.CreatedAt)
		}
		return tw.Flush()

	case "resources":
		fs, c := newFlags("google resources", "google resources CONNECTION", a.Stderr)
		pos, err := parse(fs, args)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef("google resources needs a connection id")
		}
		cl, err := a.client(c)
		if err != nil {
			return err
		}
		var resp client.Envelope[[]client.Resource]
		if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/google/resources", url.Values{"connection_id": {pos[0]}}, nil, &resp); printed || err != nil {
			return err
		}
		if len(resp.Data) == 0 {
			fmt.Fprintln(a.Stdout, "This account cannot read any sites or properties.")
			return nil
		}
		tw := newTable(a.Stdout)
		row(tw, "EXTERNAL ID", "NAME", "PERMISSION")
		for _, r := range resp.Data {
			row(tw, r.ExternalID, r.Label, r.Permission)
		}
		return tw.Flush()
	}
	return usagef("unknown google command %q (connections or resources)", sub)
}

func (a *App) api(ctx context.Context, args []string) error {
	fs, c := newFlags("api", "api [-X METHOD] [-d JSON|-] PATH", a.Stderr)
	method := fs.String("X", "", "HTTP method (default GET, or POST with -d)")
	data := fs.String("d", "", "JSON request body, or - to read it from stdin")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !strings.HasPrefix(pos[0], "/") {
		return usagef("api needs one path starting with /, e.g. api /v1/overview?period=last_7_days")
	}
	path, rawQuery, _ := strings.Cut(pos[0], "?")
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return usagef("bad query string: %v", err)
	}
	var body any
	if *data != "" {
		b := []byte(*data)
		if *data == "-" {
			if b, err = io.ReadAll(a.Stdin); err != nil {
				return err
			}
		}
		if !json.Valid(b) {
			return usagef("the request body is not valid JSON")
		}
		body = json.RawMessage(b)
	}
	m := strings.ToUpper(*method)
	if m == "" {
		m = "GET"
		if body != nil {
			m = "POST"
		}
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	raw, err := cl.Do(ctx, m, path, q, body)
	if len(raw) > 0 {
		if perr := a.printRaw(raw); perr != nil && err == nil {
			err = perr
		}
	}
	return err
}

func (a *App) mcp(args []string) error {
	fs, c := newFlags("mcp", "mcp", a.Stderr)
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	endpoint := a.apiURL(c, cfg) + "/mcp"
	snippet, _ := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"shipfolio": map[string]any{
				"url":     endpoint,
				"headers": map[string]string{"Authorization": "Bearer YOUR_API_KEY"},
			},
		},
	}, "", "  ")
	if c.json {
		_, err := fmt.Fprintln(a.Stdout, string(snippet))
		return err
	}
	fmt.Fprintf(a.Stdout, `AI agents read the same data over MCP (Streamable HTTP).

Endpoint:  %s

Clients that support MCP authorization (OAuth), such as Claude, Cursor and
VS Code, need only that URL: they open a browser where you sign in and
approve read-only access to your workspace.

Other clients send an API key:

Header:    Authorization: Bearer YOUR_API_KEY

For clients configured with an "mcpServers" file:

%s

Use a key with only the metrics:read scope for agents that should not change anything.
`, endpoint, snippet)
	return nil
}

func (a *App) listProducts(ctx context.Context, cl *client.Client) ([]client.Product, error) {
	var resp client.Envelope[[]client.Product]
	if err := cl.Call(ctx, "GET", "/v1/products", nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// resolve turns product ids, domains or names into products.
func (a *App) resolve(ctx context.Context, cl *client.Client, refs []string) ([]client.Product, error) {
	all, err := a.listProducts(ctx, cl)
	if err != nil {
		return nil, err
	}
	out := make([]client.Product, 0, len(refs))
	for _, r := range refs {
		p, err := match(all, r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func match(all []client.Product, ref string) (client.Product, error) {
	for _, p := range all {
		if p.ID == ref {
			return p, nil
		}
	}
	var hits []client.Product
	d := bareDomain(ref)
	for _, p := range all {
		if p.Domain != "" && bareDomain(p.Domain) == d {
			hits = append(hits, p)
		}
	}
	if len(hits) == 0 {
		for _, p := range all {
			if strings.EqualFold(p.Name, ref) {
				hits = append(hits, p)
			}
		}
	}
	switch len(hits) {
	case 0:
		return client.Product{}, fmt.Errorf("no product matches %q; run 'shipfolio products' to list them", ref)
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, p := range hits {
		ids[i] = p.ID
	}
	return client.Product{}, fmt.Errorf("%q matches %d products; use one of the ids: %s", ref, len(hits), strings.Join(ids, ", "))
}

func bareDomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	s, _, _ = strings.Cut(s, "/")
	return strings.TrimPrefix(s, "www.")
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

var errCancelled = errors.New("cancelled; nothing was changed")

// confirm asks before something that cannot be undone. Without a terminal
// to ask on, it requires --yes.
func (a *App) confirm(yes bool, question string) error {
	if yes {
		return nil
	}
	if !a.Interactive() {
		return usagef("%s Pass --yes to confirm.", question)
	}
	fmt.Fprintf(a.Stderr, "%s [y/N] ", question)
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errCancelled
}

func (a *App) productsRename(ctx context.Context, args []string) error {
	fs, c := newFlags("products rename", "products rename PRODUCT [--name NAME] [--domain DOMAIN] [--tags a,b]", a.Stderr)
	name := fs.String("name", "", "new name")
	domain := fs.String("domain", "", "new bare domain, e.g. example.com")
	tags := fs.String("tags", "", "comma-separated tags, replacing the current ones (\"\" clears them)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("products rename needs one product, e.g. products rename acme.com --name Acme")
	}
	body := map[string]any{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			body["name"] = *name
		case "domain":
			body["domain"] = *domain
		case "tags":
			t := splitList(*tags)
			if t == nil {
				t = []string{}
			}
			body["tags"] = t
		}
	})
	if len(body) == 0 {
		return usagef("products rename needs --name, --domain or --tags")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	ps, err := a.resolve(ctx, cl, pos)
	if err != nil {
		return err
	}
	var resp client.Envelope[client.Product]
	if printed, err := a.fetch(ctx, cl, c, "PATCH", "/v1/products/"+url.PathEscape(ps[0].ID), nil, body, &resp); printed || err != nil {
		return err
	}
	p := resp.Data
	tagText := "no tags"
	if len(p.Tags) > 0 {
		tagText = "tags " + strings.Join(p.Tags, ", ")
	}
	fmt.Fprintf(a.Stdout, "Updated %s: %s, %s, %s.\n", p.ID, p.Name, p.Domain, tagText)
	return nil
}

func (a *App) productsImport(ctx context.Context, args []string) error {
	fs, c := newFlags("products import", "products import --gsc CONN [--ga4 CONN] [--all | SITE...] [--yes]", a.Stderr)
	gsc := fs.String("gsc", "", "Search Console connection id (see 'shipfolio google connections')")
	ga4 := fs.String("ga4", "", "GA4 connection id, to match each site with a property on the same domain")
	all := fs.Bool("all", false, "import every site that has no product yet")
	yes := fs.Bool("yes", false, "create the products without asking")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *gsc == "" {
		return usagef("products import needs --gsc with a Search Console connection id (see 'shipfolio google connections')")
	}
	if *all && len(pos) > 0 {
		return usagef("give either --all or the sites to import, not both")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	q := url.Values{"gsc": {*gsc}}
	if *ga4 != "" {
		q.Set("ga4", *ga4)
	}
	var preview client.Envelope[client.ImportPreview]
	if !*all && len(pos) == 0 {
		// Only show what an import would do.
		if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/products/import", q, nil, &preview); printed || err != nil {
			return err
		}
		return a.printImportPreview(preview.Data, *ga4 != "")
	}
	if err := cl.Call(ctx, "GET", "/v1/products/import", q, nil, &preview); err != nil {
		return err
	}
	var chosen []client.ImportSite
	if *all {
		for _, s := range preview.Data.Sites {
			if !s.Exists {
				chosen = append(chosen, s)
			}
		}
	} else {
		for _, ref := range pos {
			s, err := matchSite(preview.Data.Sites, ref)
			if err != nil {
				return err
			}
			if s.Exists {
				fmt.Fprintf(a.Stderr, "%s already has a product; skipping it.\n", s.Domain)
				continue
			}
			chosen = append(chosen, s)
		}
	}
	if len(chosen) == 0 {
		fmt.Fprintln(a.Stdout, "Nothing to import: every site already has a product.")
		return nil
	}
	domains := make([]string, len(chosen))
	sites := make([]string, len(chosen))
	for i, s := range chosen {
		domains[i], sites[i] = s.Domain, s.ExternalID
	}
	if err := a.confirm(*yes, fmt.Sprintf("Create %d product(s) (%s) and start importing their history?", len(chosen), strings.Join(domains, ", "))); err != nil {
		return err
	}
	body := map[string]any{"gsc": *gsc, "sites": sites}
	if *ga4 != "" {
		body["ga4"] = *ga4
	}
	var resp client.Envelope[client.ImportResult]
	if printed, err := a.fetch(ctx, cl, c, "POST", "/v1/products/import", nil, body, &resp); printed || err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Created %d product(s). Their history is importing now; 'shipfolio products' shows how far it got.\n\n", len(resp.Data.Created))
	tw := newTable(a.Stdout)
	row(tw, "ID", "NAME", "DOMAIN", "GA4")
	for _, p := range resp.Data.Created {
		g := "no"
		if p.GA4 {
			g = "yes"
		}
		row(tw, p.ID, p.Name, p.Domain, g)
	}
	return tw.Flush()
}

func (a *App) printImportPreview(p client.ImportPreview, withGA4 bool) error {
	if len(p.Sites) == 0 {
		fmt.Fprintln(a.Stdout, "This Google account cannot read any Search Console sites.")
		return nil
	}
	tw := newTable(a.Stdout)
	head := []string{"SITE", "DOMAIN", "NAME", "STATUS"}
	if withGA4 {
		head = append(head, "GA4 PROPERTY")
	}
	row(tw, head...)
	fresh := 0
	for _, s := range p.Sites {
		status := "new"
		if s.Exists {
			status = "has a product"
		} else {
			fresh++
		}
		cells := []string{s.ExternalID, s.Domain, s.Name, status}
		if withGA4 {
			g := "no match"
			if s.GA4 != nil {
				g = s.GA4["external_id"]
				if l := s.GA4["label"]; l != "" {
					g = l + " (" + g + ")"
				}
			}
			cells = append(cells, g)
		}
		row(tw, cells...)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if p.Plan != "" {
		fmt.Fprintf(a.Stdout, "\nThe %s plan holds %d products; the workspace has %d.\n", p.Plan, p.Limit, p.Products)
	}
	if fresh > 0 {
		fmt.Fprintln(a.Stdout, "Nothing was created. Pass --all to import every new site, or name the sites or domains to import.")
	}
	return nil
}

// matchSite finds a site by its Search Console id or its domain.
func matchSite(sites []client.ImportSite, ref string) (client.ImportSite, error) {
	for _, s := range sites {
		if s.ExternalID == ref {
			return s, nil
		}
	}
	d := bareDomain(strings.TrimPrefix(ref, "sc-domain:"))
	for _, s := range sites {
		if s.Domain == d {
			return s, nil
		}
	}
	return client.ImportSite{}, fmt.Errorf("this Google account has no Search Console site %q; run without sites to list them", ref)
}

func (a *App) movers(ctx context.Context, args []string) error {
	fs, c := newFlags("movers", "movers PRODUCT query|page|country|device|channel|landing_page|bing_query|bing_page [--metric clicks] [--period 28d] [--compare year] [--limit 10] [--csv]", a.Stderr)
	metric := fs.String("metric", "", "a count to rank by: clicks, impressions, sessions, users, key_events, bing_clicks or bing_impressions (default fits the dimension)")
	period := fs.String("period", "", "7d, 28d (default), 90d or 365d")
	compare := fs.String("compare", "", "previous (default: the days just before) or year (the same days a year earlier)")
	limit := fs.Int("limit", 10, "gainers and losers to show, 1 to 50")
	asCSV := fs.Bool("csv", false, "print CSV")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef("movers needs a product and a dimension (query, page, country, device, channel, landing_page, bing_query or bing_page)")
	}
	if *limit < 1 || *limit > 50 {
		return usagef("--limit must be between 1 and 50")
	}
	q := url.Values{"dimension": {pos[1]}, "limit": {strconv.Itoa(*limit)}}
	if *metric != "" {
		q.Set("metric", *metric)
	}
	if err := rangeQuery(q, *period, "", ""); err != nil {
		return err
	}
	cmp, err := compareValue(*compare)
	if err != nil {
		return err
	}
	if cmp != "" {
		q.Set("compare", cmp)
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	products, err := a.resolve(ctx, cl, pos[:1])
	if err != nil {
		return err
	}
	q.Set("product_id", products[0].ID)
	var resp client.Envelope[client.Movers]
	if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/metrics/movers", q, nil, &resp); printed || err != nil {
		return err
	}
	m := resp.Data
	if *asCSV {
		w := csv.NewWriter(a.Stdout)
		_ = w.Write([]string{"direction", m.Dimension, "previous", "current", "delta", "change"})
		for _, part := range []struct {
			name string
			rows []client.Mover
		}{{"gainer", m.Gainers}, {"loser", m.Losers}} {
			for _, r := range part.rows {
				prev, cur, delta := r.Previous, r.Current, r.Delta
				_ = w.Write([]string{part.name, r.Value, csvValue(&prev), csvValue(&cur), csvValue(&delta), csvValue(r.Change)})
			}
		}
		w.Flush()
		return w.Error()
	}
	p := products[0]
	dim := strings.ReplaceAll(m.Dimension, "_", " ")
	fmt.Fprintf(a.Stdout, "%s, %s by %s, %s to %s against %s\n", productLabel(p.Name, p.Domain), dim, strings.ReplaceAll(m.Metric, "_", " "), m.Period.Start, m.Period.End, againstText(m.Period))
	for _, part := range []struct {
		title string
		rows  []client.Mover
	}{{"Gainers", m.Gainers}, {"Losers", m.Losers}} {
		fmt.Fprintf(a.Stdout, "\n%s\n", part.title)
		if len(part.rows) == 0 {
			fmt.Fprintln(a.Stdout, "None.")
			continue
		}
		tw := newTable(a.Stdout)
		row(tw, strings.ToUpper(dim), "PREVIOUS", "CURRENT", "DELTA", "CHANGE")
		for _, r := range part.rows {
			prev, cur := r.Previous, r.Current
			row(tw, r.Value, fmtMetric(m.Metric, &prev), fmtMetric(m.Metric, &cur), fmtDelta(r.Delta), fmtChange(r.Change))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) notes(ctx context.Context, args []string) error {
	sub := "list"
	if len(args) > 0 {
		switch args[0] {
		case "list", "ls", "add", "rm", "delete", "remove":
			sub, args = args[0], args[1:]
		}
	}
	switch sub {
	case "add":
		return a.noteAdd(ctx, args)
	case "rm", "delete", "remove":
		return a.noteRemove(ctx, args)
	}
	return a.notesList(ctx, args)
}

func (a *App) notesList(ctx context.Context, args []string) error {
	fs, c := newFlags("notes", "notes [--product PRODUCT] [--start DATE --end DATE]", a.Stderr)
	product := fs.String("product", "", "only notes on this product (and those on every product)")
	start := fs.String("start", "", "first day, YYYY-MM-DD")
	end := fs.String("end", "", "last day, YYYY-MM-DD, inclusive")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("unknown notes command %q (list, add or rm)", pos[0])
	}
	q := url.Values{}
	for name, v := range map[string]string{"start": *start, "end": *end} {
		if v == "" {
			continue
		}
		if !validDate(v) {
			return usagef("--%s must be YYYY-MM-DD", name)
		}
		q.Set(name, v)
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	all, err := a.listProducts(ctx, cl)
	if err != nil {
		return err
	}
	if *product != "" {
		p, err := match(all, *product)
		if err != nil {
			return err
		}
		q.Set("product_id", p.ID)
	}
	var resp client.Envelope[[]client.Annotation]
	if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/annotations", q, nil, &resp); printed || err != nil {
		return err
	}
	if len(resp.Data) == 0 {
		fmt.Fprintln(a.Stdout, "No notes. Add one with 'shipfolio note add today \"Launched v2\"'.")
		return nil
	}
	names := map[string]string{}
	for _, p := range all {
		names[p.ID] = productLabel(p.Name, p.Domain)
	}
	tw := newTable(a.Stdout)
	row(tw, "ID", "DATE", "PRODUCT", "NOTE", "BY")
	for _, n := range resp.Data {
		name := "all products"
		if n.ProductID != "" {
			if name = names[n.ProductID]; name == "" {
				name = n.ProductID
			}
		}
		row(tw, n.ID, n.Date, name, n.Text, n.CreatedBy)
	}
	return tw.Flush()
}

func (a *App) noteAdd(ctx context.Context, args []string) error {
	fs, c := newFlags("note add", "note add [--product PRODUCT] DATE|today TEXT", a.Stderr)
	product := fs.String("product", "", "the product the note is about (default: every product)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef("note add needs a date and the text, e.g. note add today \"Launched v2\"")
	}
	date, err := a.dateValue(pos[0])
	if err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(pos[1:], " "))
	if text == "" || len([]rune(text)) > 280 {
		return usagef("the note must be 1 to 280 characters")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	body := map[string]any{"date": date, "text": text}
	label := "every product"
	if *product != "" {
		ps, err := a.resolve(ctx, cl, []string{*product})
		if err != nil {
			return err
		}
		body["product_id"] = ps[0].ID
		label = productLabel(ps[0].Name, ps[0].Domain)
	}
	var resp client.Envelope[client.Annotation]
	if printed, err := a.fetch(ctx, cl, c, "POST", "/v1/annotations", nil, body, &resp); printed || err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Added note %s on %s for %s.\n", resp.Data.ID, resp.Data.Date, label)
	return nil
}

func (a *App) noteRemove(ctx context.Context, args []string) error {
	fs, c := newFlags("note rm", "note rm ID [--yes]", a.Stderr)
	yes := fs.Bool("yes", false, "delete without asking")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("note rm needs one note id (see 'shipfolio notes')")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	question := fmt.Sprintf("Delete note %s?", pos[0])
	var list client.Envelope[[]client.Annotation]
	if err := cl.Call(ctx, "GET", "/v1/annotations", nil, nil, &list); err == nil {
		for _, n := range list.Data {
			if n.ID == pos[0] {
				question = fmt.Sprintf("Delete the note of %s, %q?", n.Date, n.Text)
			}
		}
	}
	if err := a.confirm(*yes, question); err != nil {
		return err
	}
	var resp client.Envelope[map[string]any]
	if printed, err := a.fetch(ctx, cl, c, "DELETE", "/v1/annotations/"+url.PathEscape(pos[0]), nil, nil, &resp); printed || err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Deleted note %s.\n", pos[0])
	return nil
}

// dateValue reads YYYY-MM-DD, today or yesterday (on this computer's clock).
func (a *App) dateValue(s string) (string, error) {
	switch strings.ToLower(s) {
	case "today":
		return a.now().Format(time.DateOnly), nil
	case "yesterday":
		return a.now().AddDate(0, 0, -1).Format(time.DateOnly), nil
	}
	if !validDate(s) {
		return "", usagef("the date must be YYYY-MM-DD, today or yesterday, not %q", s)
	}
	return s, nil
}

func validDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}
