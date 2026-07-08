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
		// Sort by raw failure count, not fail-rate: a throttled IP is pulled
		// from rotation after one 403/429, so it makes only a request or two
		// and its rate is ~100% regardless — the count is the real signal of
		// which IPs are getting blocked most.
		sort.Slice(a.ips, func(i, j int) bool {
			if a.ips[i].Failed != a.ips[j].Failed {
				return a.ips[i].Failed > a.ips[j].Failed
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

// FormatReport renders a per-provider request summary as a plain-text
// Discord message. Each target shows its overall success/failure split,
// the IPs that saw failures (by count — see the sort note), and a rollup
// of the healthy IPs so successes are always visible. Shared by the daily
// problemos post and the on-demand /schniff ratelimits command.
func FormatReport(stats []EndpointStat, since, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📊 **Proxy request report** · last %s\n", humanDur(now.Sub(since)))

	targets := aggregateByTarget(stats)
	if len(targets) == 0 {
		b.WriteString("No proxy traffic in this window.")
		return b.String()
	}

	const maxIPsPerTarget = 10
	for _, t := range targets {
		ok := t.requests - t.failed
		fmt.Fprintf(&b, "\n**%s** — %s ok / %s failed (%.1f%% of %s)\n",
			t.name, comma(ok), comma(t.failed), pct(t.failed, t.requests), comma(t.requests))

		// Split IPs into ones that saw failures vs fully-healthy ones, so we
		// can list the offenders and still account for the successes.
		var failing []EndpointStat
		var healthyIPs, healthyReq int64
		for _, ip := range t.ips {
			if ip.Failed > 0 {
				failing = append(failing, ip)
			} else {
				healthyIPs++
				healthyReq += ip.Requests
			}
		}

		shown := 0
		for _, ip := range failing {
			if shown >= maxIPsPerTarget {
				fmt.Fprintf(&b, "• …and %d more IPs with failures\n", len(failing)-shown)
				break
			}
			// Show counts (failed of total), not a standalone rate: a
			// cooled-down IP's rate is ~100% off one or two attempts, which
			// reads scarier than it is.
			fmt.Fprintf(&b, "• %s — %s failed of %s\n",
				ipLabel(ip), comma(ip.Failed), reqWord(ip.Requests))
			shown++
		}
		if healthyIPs > 0 {
			word := "other IPs"
			if len(failing) == 0 {
				word = "all IPs"
			}
			if healthyIPs == 1 {
				word = strings.Replace(word, "IPs", "IP", 1)
			}
			fmt.Fprintf(&b, "• %d %s healthy — %s, no failures\n",
				healthyIPs, word, reqWord(healthyReq))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// reqWord formats a request count with its unit, e.g. "1 request" / "512 requests".
func reqWord(n int64) string {
	if n == 1 {
		return "1 request"
	}
	return comma(n) + " requests"
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
