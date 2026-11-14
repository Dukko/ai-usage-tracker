package web

import (
	"fmt"
	"html/template"
	"math"
	"strings"
	"time"

	"ai-usage-tracker/internal/providers"
	"ai-usage-tracker/internal/store"
)

type quotaView struct {
	Window      string
	UsedPercent float64
	ResetsAt    int64
	ResetsIn    string
	Level       string
	Style       template.CSS
	Spark       template.HTML
}

type accountView struct {
	store.Account
	ProviderLabel string
	IsOAuth       bool
	IsPush        bool
	Quotas        []quotaView
	CostToday     float64
	Cost30        float64
	Tokens30      int64
	Chart         template.HTML
	Days          []store.DayTotal // newest first, detail page only
	Models        []store.ModelTotal
}

func (s *Server) buildView(a store.Account, detail bool) (accountView, error) {
	v := accountView{Account: a, ProviderLabel: a.Provider}
	prov, ok := providers.Get(a.Provider)
	if ok {
		v.ProviderLabel = prov.Label()
		v.IsOAuth = prov.Kind() == providers.KindOAuth
		v.IsPush = prov.Kind() == providers.KindPush
	}

	// Subscriptions report only quotas; some API providers (OpenRouter key limits) report them too.
	quotas, err := s.st.LatestQuotas(a.ID)
	if err != nil {
		return v, err
	}
	{
		histDays := 7
		if detail {
			histDays = 30
		}
		for _, q := range quotas {
			qv := quotaView{
				Window:      q.Window,
				UsedPercent: q.UsedPercent,
				ResetsAt:    q.ResetsAt,
				ResetsIn:    until(q.ResetsAt),
				Level:       level(q.UsedPercent),
				Style:       template.CSS(fmt.Sprintf("width:%.1f%%", math.Max(0, math.Min(100, q.UsedPercent)))),
			}
			hist, err := s.st.QuotaHistory(a.ID, q.Window, time.Now().AddDate(0, 0, -histDays))
			if err != nil {
				return v, err
			}
			qv.Spark = sparkline(downsample(hist, 160), 100)
			v.Quotas = append(v.Quotas, qv)
		}
	}
	if v.IsOAuth {
		return v, nil
	}

	days, err := s.st.DailyTotals(a.ID, 30)
	if err != nil {
		return v, err
	}
	vals := make([]float64, len(days))
	var anyCost bool
	for _, d := range days {
		v.Cost30 += d.USD
		v.Tokens30 += d.Tokens()
		anyCost = anyCost || d.USD > 0
	}
	for i, d := range days {
		if anyCost {
			vals[i] = d.USD
		} else {
			vals[i] = float64(d.Tokens())
		}
	}
	if n := len(days); n > 0 {
		v.CostToday = days[n-1].USD
	}
	v.Chart = bars(vals, days, anyCost)

	if detail {
		for i := len(days) - 1; i >= 0; i-- {
			v.Days = append(v.Days, days[i])
		}
		if v.Models, err = s.st.ModelTotals(a.ID, 30); err != nil {
			return v, err
		}
	}
	return v, nil
}

func level(pct float64) string {
	switch {
	case pct >= 90:
		return "crit"
	case pct >= 70:
		return "warn"
	default:
		return "ok"
	}
}

// downsample keeps at most n points, taking the max of each bucket so spikes survive.
func downsample(vals []float64, n int) []float64 {
	if len(vals) <= n {
		return vals
	}
	out := make([]float64, n)
	for i := range out {
		lo, hi := i*len(vals)/n, (i+1)*len(vals)/n
		m := vals[lo]
		for _, x := range vals[lo:hi] {
			m = math.Max(m, x)
		}
		out[i] = m
	}
	return out
}

func sparkline(vals []float64, max float64) template.HTML {
	if len(vals) < 2 {
		return ""
	}
	const w, h = 200.0, 32.0
	var pts strings.Builder
	for i, x := range vals {
		px := float64(i) * w / float64(len(vals)-1)
		py := h - 1 - math.Min(x, max)/max*(h-2)
		fmt.Fprintf(&pts, "%.1f,%.1f ", px, py)
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="spark" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" aria-hidden="true"><polyline points="%s"/></svg>`,
		w, h, strings.TrimSpace(pts.String())))
}

func bars(vals []float64, days []store.DayTotal, money bool) template.HTML {
	if len(vals) == 0 {
		return ""
	}
	max := 0.0
	for _, x := range vals {
		max = math.Max(max, x)
	}
	if max == 0 {
		return template.HTML(`<p class="muted small">No usage in the last 30 days.</p>`)
	}
	const h = 48.0
	w := float64(len(vals)) * 10
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="bars" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="Daily usage, last 30 days">`, w, h)
	for i, x := range vals {
		bh := x / max * (h - 1)
		if x > 0 && bh < 1 {
			bh = 1
		}
		label := tokens(int64(x))
		if money {
			label = fmt.Sprintf("$%.2f", x)
		}
		fmt.Fprintf(&b, `<rect x="%d" y="%.1f" width="8" height="%.1f"><title>%s: %s</title></rect>`,
			i*10+1, h-bh, bh, template.HTMLEscapeString(days[i].Day), template.HTMLEscapeString(label))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func until(unix int64) string {
	if unix == 0 {
		return ""
	}
	d := time.Until(time.Unix(unix, 0))
	if d <= 0 {
		return "now"
	}
	return "in " + human(d)
}

func ago(unix int64) string {
	if unix == 0 {
		return "never"
	}
	d := time.Since(time.Unix(unix, 0))
	if d < time.Minute {
		return "just now"
	}
	return human(d) + " ago"
}

func human(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dm", int(math.Ceil(d.Minutes())))
	}
}

func tokens(n int64) string {
	f := float64(n)
	switch {
	case f >= 1e9:
		return fmt.Sprintf("%.2fB", f/1e9)
	case f >= 1e6:
		return fmt.Sprintf("%.1fM", f/1e6)
	case f >= 1e3:
		return fmt.Sprintf("%.1fk", f/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

var funcs = template.FuncMap{
	"money":  func(f float64) string { return fmt.Sprintf("$%.2f", f) },
	"tokens": tokens,
	"ago":    ago,
	"pct":    func(f float64) string { return fmt.Sprintf("%.0f%%", f) },
	"deref":  func(f *float64) float64 { return *f },
}
