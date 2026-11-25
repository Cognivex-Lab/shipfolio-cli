package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/Cognivex-Lab/shipfolio-cli/internal/client"
)

// SEO and GEO help: tasks to change on the site (what audits, index checks
// and suggestions found), the site audit, and Claude's suggestions for a
// page. A coding agent reads tasks here or over MCP, makes the change and
// marks the task done.

func (a *App) tasks(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "list", "ls":
			return a.tasksList(ctx, args[1:])
		case "show":
			return a.taskShow(ctx, args[1:])
		case "add":
			return a.taskAdd(ctx, args[1:])
		case "done", "dismiss", "reopen":
			return a.taskSet(ctx, args[0], args[1:])
		}
	}
	return a.tasksList(ctx, args)
}

var taskStatuses = map[string]bool{"open": true, "done": true, "dismissed": true, "all": true}

func (a *App) tasksList(ctx context.Context, args []string) error {
	fs, c := newFlags("tasks", "tasks [--product PRODUCT] [--status open|done|dismissed|all] [--fix]", a.Stderr)
	product := fs.String("product", "", "only this product")
	status := fs.String("status", "open", "open, done, dismissed or all")
	withFix := fs.Bool("fix", false, "print how to fix each task")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("tasks takes no arguments; use --product")
	}
	if !taskStatuses[*status] {
		return usagef("--status must be open, done, dismissed or all, not %q", *status)
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	all, err := a.listProducts(ctx, cl)
	if err != nil {
		return err
	}
	q := url.Values{"status": {*status}}
	if *product != "" {
		p, err := match(all, *product)
		if err != nil {
			return err
		}
		q.Set("product_id", p.ID)
	}
	var resp client.Envelope[[]client.Task]
	if printed, err := a.fetch(ctx, cl, c, "GET", "/v1/tasks", q, nil, &resp); printed || err != nil {
		return err
	}
	if len(resp.Data) == 0 {
		fmt.Fprintf(a.Stdout, "No %s tasks.\n", map[string]string{"open": "open", "done": "done", "dismissed": "dismissed", "all": ""}[*status])
		return nil
	}
	names := map[string]string{}
	for _, p := range all {
		names[p.ID] = productLabel(p.Name, p.Domain)
	}
	if *withFix {
		for i, t := range resp.Data {
			if i > 0 {
				fmt.Fprintln(a.Stdout)
			}
			a.printTask(t, names[t.ProductID])
		}
		return nil
	}
	tw := newTable(a.Stdout)
	row(tw, "ID", "PRODUCT", "LEVEL", "TASK", "WHERE", "STATUS")
	for _, t := range resp.Data {
		row(tw, t.ID, names[t.ProductID], dash(t.Level), t.Title, where(t), taskStatus(t))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if *status == "open" {
		fmt.Fprintln(a.Stdout, "\nRun 'shipfolio tasks show ID' for how to fix one, and 'shipfolio tasks done ID' once it is.")
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func where(t client.Task) string {
	parts := []string{}
	if t.URL != "" {
		parts = append(parts, t.URL)
	}
	if t.Query != "" {
		parts = append(parts, fmt.Sprintf("%q", t.Query))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " ")
}

func taskStatus(t client.Task) string {
	if t.Status != "done" {
		return t.Status
	}
	s := "done " + t.DoneOn
	if o := t.Outcome; o != nil && o.Ready && o.ClicksChange != nil {
		s += " (clicks " + fmtChange(o.ClicksChange) + ")"
	} else if o != nil && !o.Ready {
		s += fmt.Sprintf(" (measuring, %d/28 days)", o.DaysAfter)
	}
	return s
}

func (a *App) printTask(t client.Task, product string) {
	fmt.Fprintf(a.Stdout, "%s  %s\n", t.ID, t.Title)
	if product != "" {
		fmt.Fprintf(a.Stdout, "Product: %s\n", product)
	}
	fmt.Fprintf(a.Stdout, "Status:  %s · from %s", taskStatus(t), t.Source)
	if t.Level != "" {
		fmt.Fprintf(a.Stdout, " · %s", t.Level)
	}
	fmt.Fprintln(a.Stdout)
	if w := where(t); w != "—" {
		fmt.Fprintf(a.Stdout, "Where:   %s\n", w)
	}
	if t.Detail != "" {
		fmt.Fprintf(a.Stdout, "\n%s\n", t.Detail)
	}
	if t.Fix != "" {
		fmt.Fprintf(a.Stdout, "\nHow to fix it:\n%s\n", indent(t.Fix))
	}
	if o := t.Outcome; o != nil && o.Before != nil && o.After != nil {
		fmt.Fprintf(a.Stdout, "\nOutcome for the %s: %s clicks in the 28 days before, %s in the %d after", o.Basis, fmtCount(o.Before.Clicks), fmtCount(o.After.Clicks), o.DaysAfter)
		if o.ClicksChange != nil {
			fmt.Fprintf(a.Stdout, " (%s)", fmtChange(o.ClicksChange))
		}
		fmt.Fprintln(a.Stdout, ".")
	}
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// findTask looks a task up among every status, for show and its product.
func (a *App) findTask(ctx context.Context, cl *client.Client, id string) (client.Task, error) {
	var resp client.Envelope[[]client.Task]
	if err := cl.Call(ctx, "GET", "/v1/tasks", url.Values{"status": {"all"}}, nil, &resp); err != nil {
		return client.Task{}, err
	}
	for _, t := range resp.Data {
		if t.ID == id {
			return t, nil
		}
	}
	return client.Task{}, fmt.Errorf("no task %s in this workspace (see 'shipfolio tasks --status all')", id)
}

func (a *App) taskShow(ctx context.Context, args []string) error {
	fs, c := newFlags("tasks show", "tasks show ID", a.Stderr)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("tasks show needs one task id (see 'shipfolio tasks')")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	t, err := a.findTask(ctx, cl, pos[0])
	if err != nil {
		return err
	}
	if c.json {
		raw, err := json.Marshal(t)
		if err != nil {
			return err
		}
		return a.printRaw(raw)
	}
	all, err := a.listProducts(ctx, cl)
	if err != nil {
		return err
	}
	name := t.ProductID
	for _, p := range all {
		if p.ID == t.ProductID {
			name = productLabel(p.Name, p.Domain)
		}
	}
	a.printTask(t, name)
	return nil
}

func (a *App) taskSet(ctx context.Context, verb string, args []string) error {
	fs, c := newFlags("tasks "+verb, "tasks "+verb+" ID...", a.Stderr)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef("tasks %s needs at least one task id (see 'shipfolio tasks')", verb)
	}
	status := map[string]string{"done": "done", "dismiss": "dismissed", "reopen": "open"}[verb]
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	for _, id := range pos {
		var resp client.Envelope[client.Task]
		if printed, err := a.fetch(ctx, cl, c, "PATCH", "/v1/tasks/"+url.PathEscape(id), nil, map[string]string{"status": status}, &resp); err != nil {
			return err
		} else if printed {
			continue
		}
		switch status {
		case "done":
			fmt.Fprintf(a.Stdout, "Done: %s. Shipfolio noted it on the charts and will compare the 28 days after with the 28 before.\n", resp.Data.Title)
		case "dismissed":
			fmt.Fprintf(a.Stdout, "Dismissed: %s.\n", resp.Data.Title)
		default:
			fmt.Fprintf(a.Stdout, "Reopened: %s.\n", resp.Data.Title)
		}
	}
	return nil
}

func (a *App) taskAdd(ctx context.Context, args []string) error {
	fs, c := newFlags("tasks add", "tasks add PRODUCT TITLE [--url URL] [--query QUERY] [--fix TEXT]", a.Stderr)
	pageURL := fs.String("url", "", "the page it is about")
	query := fs.String("query", "", "the search query it is about")
	fix := fs.String("fix", "", "how to do it")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef("tasks add needs a product and what to do, e.g. tasks add alpha.com \"Rewrite the pricing page title\"")
	}
	title := strings.TrimSpace(strings.Join(pos[1:], " "))
	if len([]rune(title)) > 200 {
		return usagef("the task is at most 200 characters; put the details in --fix")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	ps, err := a.resolve(ctx, cl, pos[:1])
	if err != nil {
		return err
	}
	body := map[string]any{"product_id": ps[0].ID, "title": title, "source": "manual"}
	for k, v := range map[string]string{"url": *pageURL, "query": *query, "fix": *fix} {
		if v != "" {
			body[k] = v
		}
	}
	var resp client.Envelope[client.Task]
	if printed, err := a.fetch(ctx, cl, c, "POST", "/v1/tasks", nil, body, &resp); printed || err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "Added task %s for %s.\n", resp.Data.ID, productLabel(ps[0].Name, ps[0].Domain))
	return nil
}

// ---- the site audit ----

func (a *App) audit(ctx context.Context, args []string) error {
	fs, c := newFlags("audit", "audit PRODUCT [--run] [--all]", a.Stderr)
	run := fs.Bool("run", false, "audit the site now instead of showing the last audit")
	all := fs.Bool("all", false, "list passed checks too")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("audit needs one product")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	ps, err := a.resolve(ctx, cl, pos)
	if err != nil {
		return err
	}
	method, path, q := "GET", "/v1/site-audit", url.Values{"product_id": {ps[0].ID}}
	if *run {
		method, path, q = "POST", "/v1/products/"+url.PathEscape(ps[0].ID)+"/site-audit", nil
		if !c.json {
			fmt.Fprintf(a.Stderr, "Auditing %s…\n", ps[0].Domain)
		}
	}
	var resp client.Envelope[client.SiteAudit]
	if printed, err := a.fetch(ctx, cl, c, method, path, q, nil, &resp); printed || err != nil {
		return err
	}
	r := resp.Data.Audit
	if r == nil {
		fmt.Fprintf(a.Stdout, "%s has not been audited yet. Run 'shipfolio audit %s --run'.\n", ps[0].Domain, pos[0])
		return nil
	}
	fmt.Fprintf(a.Stdout, "%s · score %d/100 · checked %s\n", ps[0].Domain, r.Score, strings.SplitN(r.CheckedAt, "T", 2)[0])
	blocked := []string{}
	for _, cr := range r.Crawlers {
		if !cr.Allowed {
			blocked = append(blocked, cr.Agent+" ("+cr.Purpose+")")
		}
	}
	if len(blocked) > 0 {
		fmt.Fprintf(a.Stdout, "Blocked in robots.txt: %s\n", strings.Join(blocked, ", "))
	} else {
		fmt.Fprintln(a.Stdout, "No crawler is blocked in robots.txt.")
	}
	fmt.Fprintln(a.Stdout)
	tw := newTable(a.Stdout)
	row(tw, "LEVEL", "FINDING", "WHERE")
	shown := 0
	for _, ch := range r.Checks {
		if ch.Level == "pass" && !*all {
			continue
		}
		row(tw, ch.Level, ch.Title, dash(ch.URL))
		shown++
	}
	if shown == 0 {
		row(tw, "pass", "Nothing to fix", "—")
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(a.Stdout, "\nEach problem is also a task with its fix: 'shipfolio tasks --product "+pos[0]+" --fix'.")
	return nil
}

// ---- suggestions for a page ----

func (a *App) suggest(ctx context.Context, args []string) error {
	fs, c := newFlags("suggest", "suggest PRODUCT URL [--lang en|es|zh-CN]", a.Stderr)
	lang := fs.String("lang", "en", "language: en, es or zh-CN")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return usagef("suggest needs a product and a page address on its domain")
	}
	if *lang != "en" && *lang != "es" && *lang != "zh-CN" {
		return usagef("--lang must be en, es or zh-CN")
	}
	cl, err := a.client(c)
	if err != nil {
		return err
	}
	ps, err := a.resolve(ctx, cl, pos[:1])
	if err != nil {
		return err
	}
	if !c.json {
		fmt.Fprintf(a.Stderr, "Reading %s and asking Claude…\n", pos[1])
	}
	var resp client.Envelope[client.PageSuggestions]
	if printed, err := a.fetch(ctx, cl, c, "POST", "/v1/products/"+url.PathEscape(ps[0].ID)+"/page-suggestions", nil,
		map[string]string{"url": pos[1], "lang": *lang}, &resp); printed || err != nil {
		return err
	}
	s := resp.Data.Suggestions
	out := a.Stdout
	fmt.Fprintf(out, "%s\n\n%s\n", resp.Data.URL, s.Summary)
	for _, r := range []struct {
		label string
		rw    client.Rewrite
	}{{"Title", s.Title}, {"Description", s.Description}, {"H1", s.H1}} {
		if r.rw.Text == "" {
			continue
		}
		fmt.Fprintf(out, "\n%s: %s\n  why: %s\n", r.label, r.rw.Text, r.rw.Why)
	}
	for _, sec := range s.Sections {
		fmt.Fprintf(out, "\nAdd: %s\n", sec.Heading)
		for _, p := range sec.Points {
			fmt.Fprintf(out, "  - %s\n", p)
		}
		fmt.Fprintf(out, "  why: %s\n", sec.Why)
	}
	if len(s.FAQ) > 0 {
		fmt.Fprintln(out, "\nQuestions to answer:")
		for _, f := range s.FAQ {
			fmt.Fprintf(out, "  Q: %s\n  A: %s\n", f.Question, f.Answer)
		}
	}
	for _, sc := range s.Schema {
		fmt.Fprintf(out, "\nStructured data: %s · %s\n", sc.Type, sc.Why)
	}
	return nil
}
