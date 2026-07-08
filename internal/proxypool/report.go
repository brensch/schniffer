package proxypool

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// targetAgg groups one provider's per-IP stats for reporting.
type targetAgg struct {
	name     string
	requests int64
	failed   int64
	ips      []EndpointStat
}

// aggregateByTarget rolls the flat per-(target, endpoint) stats up into one
// group per provider, sorted worst-failure-rate first.
func aggregateByTarget(stats []EndpointStat) []targetAgg {
	byTarget := map[string]*targetAgg{}
	for _, s := range stats {
		a := byTarget[s.Target]
		if a == nil {
			a = &targetAgg{name: s.Target}
			byTarget[s.Target] = a
		}
		a.requests += s.Requests
		a.failed += s.Failed
		a.ips = append(a.ips, s)
	}
	out := make([]targetAgg, 0, len(byTarget))
	for _, a := range byTarget {
		sort.Slice(a.ips, func(i, j int) bool {
			ri := failRate(a.ips[i])
			rj := failRate(a.ips[j])
			if ri != rj {
				return ri > rj
			}
			return a.ips[i].Region < a.ips[j].Region
		})
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		ri := pct(out[i].failed, out[i].requests)
		rj := pct(out[j].failed, out[j].requests)
		if ri != rj {
			return ri > rj
		}
		return out[i].name < out[j].name
	})
	return out
}

// humanDur renders a duration as "45m", "3h", or "3h12m" — no dangling
// "0m0s" tail from Duration.String().
func humanDur(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

func pct(n, d int64) float64 {
	if d == 0 {
		return 0
	}
	return 100 * float64(n) / float64(d)
}

func failRate(s EndpointStat) float64 { return pct(s.Failed, s.Requests) }

// FormatReport renders a per-provider rate-limit summary as a plain-text
// Discord message: one section per target, each with an overall failure
// rate and a line per IP that saw failures. Shared by the daily problemos
// post and the on-demand /schniff ratelimits command.
func FormatReport(stats []EndpointStat, since, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📊 **Proxy failure report** · last %s\n", humanDur(now.Sub(since)))

	targets := aggregateByTarget(stats)
	if len(targets) == 0 {
		b.WriteString("No proxy traffic in this window.")
		return b.String()
	}

	const maxIPsPerTarget = 10
	for _, t := range targets {
		fmt.Fprintf(&b, "\n**%s** — %.1f%% failed (%s / %s requests)\n",
			t.name, pct(t.failed, t.requests), comma(t.failed), comma(t.requests))

		// Count failing IPs first so we can honestly note any we truncate.
		failing := 0
		for _, ip := range t.ips {
			if ip.Failed > 0 {
				failing++
			}
		}
		if failing == 0 {
			b.WriteString("• all IPs healthy\n")
			continue
		}
		shown := 0
		for _, ip := range t.ips {
			if ip.Failed == 0 {
				continue
			}
			if shown >= maxIPsPerTarget {
				fmt.Fprintf(&b, "• …and %d more IPs with failures\n", failing-shown)
				break
			}
			fmt.Fprintf(&b, "• %s — %.0f%% failed (%s / %s)\n",
				ipLabel(ip), failRate(ip), comma(ip.Failed), comma(ip.Requests))
			shown++
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ipLabel prefers the human region name, falling back to the endpoint URL.
func ipLabel(s EndpointStat) string {
	if s.Region != "" {
		return s.Region
	}
	return strings.TrimPrefix(s.URL, "https://")
}

// comma formats n with thousands separators (e.g. 8420 -> "8,420").
func comma(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return "-" + comma(-n)
	}
	if len(s) <= 3 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	return strings.Join(parts, ",")
}
