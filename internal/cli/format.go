package cli

import (
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/Cognivex-Lab/shipfolio-cli/internal/client"
)

// none stands for a value Google did not report. It must never read as 0.
const none = "—"

func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
}

func row(tw io.Writer, cells ...string) {
	io.WriteString(tw, strings.Join(cells, "\t")+"\n")
}

// metricOrder is how metric columns are arranged; unknown metrics follow
// in alphabetical order.
var metricOrder = []string{
	"clicks", "impressions", "ctr", "avg_position",
	"organic_clicks", "organic_impressions",
	"sessions", "users", "engagement_rate", "key_events",
	"bing_clicks", "bing_impressions", "bing_ctr", "bing_avg_position",
}

func sortMetrics(names []string) []string {
	rank := map[string]int{}
	for i, n := range metricOrder {
		rank[n] = i
	}
	out := append([]string(nil), names...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, oki := rank[out[i]]
		rj, okj := rank[out[j]]
		switch {
		case oki && okj:
			return ri < rj
		case oki != okj:
			return oki
		default:
			return out[i] < out[j]
		}
	})
	return out
}

func metricTitle(name string) string {
	switch name {
	case "ctr":
		return "CTR"
	case "avg_position":
		return "POSITION"
	case "engagement_rate":
		return "ENGAGEMENT"
	case "bing_ctr":
		return "BING CTR"
	case "bing_avg_position":
		return "BING POSITION"
	}
	return strings.ToUpper(strings.ReplaceAll(name, "_", " "))
}

// fmtMetric formats one value the way the dashboard does: rates as
// percentages, position to one decimal, counts with digit grouping.
func fmtMetric(name string, v *float64) string {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return none
	}
	switch name {
	case "ctr", "engagement_rate", "bing_ctr":
		return strconv.FormatFloat(*v*100, 'f', 1, 64) + "%"
	case "avg_position", "bing_avg_position":
		return strconv.FormatFloat(*v, 'f', 1, 64)
	}
	return fmtCount(*v)
}

func fmtCount(v float64) string {
	if v != math.Trunc(v) || math.Abs(v) >= 1e15 {
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	s := strconv.FormatInt(int64(math.Abs(v)), 10)
	var b strings.Builder
	if v < 0 {
		b.WriteByte('-')
	}
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fmtDelta formats an absolute change with its sign: +1,200 or -35.
func fmtDelta(v float64) string {
	s := fmtCount(v)
	if v > 0 {
		s = "+" + s
	}
	return s
}

// fmtChange formats a relative change such as -0.12 as "-12.0%".
func fmtChange(v *float64) string {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return none
	}
	s := strconv.FormatFloat(*v*100, 'f', 1, 64) + "%"
	if *v > 0 {
		s = "+" + s
	}
	return s
}

// csvValue writes null as an empty cell, which spreadsheets read as blank
// rather than zero.
func csvValue(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// againstText describes the range a period is compared with.
func againstText(p client.Period) string {
	s := p.PrevStart + " to " + p.PrevEnd
	if p.Compare == "year" {
		s += " (a year earlier)"
	}
	return s
}

func productLabel(name, domain string) string {
	if domain != "" {
		return domain
	}
	return name
}
