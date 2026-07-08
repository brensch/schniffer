package proxypool

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// FormatReport renders a per-IP rate-limit summary as a Discord message.
// Offenders (any 429/403) are listed most-throttled first in a code block;
// a clean window collapses to a one-liner. Capped so the message stays
// under Discord's 2000-char limit even with every IP degraded. Shared by
// the daily problemos post and the on-demand /schniff ratelimits command.
func FormatReport(stats []EndpointStat, since, now time.Time) string {
	var totReq, totRL, tot403, totCD int64
	for _, s := range stats {
		totReq += s.Requests
		totRL += s.RateLimited
		tot403 += s.Forbidden
		totCD += s.Cooldowns
	}

	var b strings.Builder
	window := now.Sub(since).Round(time.Minute)
	fmt.Fprintf(&b, "📊 **Proxy rate-limit report** (last %s)\n", window)
	if totReq == 0 {
		b.WriteString("No proxy traffic recorded in this window.")
		return b.String()
	}
	fmt.Fprintf(&b, "%d IPs · %d requests · %d rate-limited (429) · %d blocked (403) · %d cooldowns — %.1f%% throttled overall.\n",
		len(stats), totReq, totRL, tot403, totCD,
		100*float64(totRL+tot403)/float64(totReq))

	offenders := make([]EndpointStat, 0, len(stats))
	for _, s := range stats {
		if s.RateLimited+s.Forbidden > 0 {
			offenders = append(offenders, s)
		}
	}
	if len(offenders) == 0 {
		b.WriteString("No individual IP hit a 429 or 403. 🎉")
		return b.String()
	}
	sort.Slice(offenders, func(i, j int) bool {
		oi := offenders[i].RateLimited + offenders[i].Forbidden
		oj := offenders[j].RateLimited + offenders[j].Forbidden
		if oi != oj {
			return oi > oj
		}
		return offenders[i].Region < offenders[j].Region
	})

	b.WriteString("```\n")
	fmt.Fprintf(&b, "%-22s %7s %6s %6s %5s\n", "region", "reqs", "429", "403", "cool")
	const maxRows = 20
	for i, s := range offenders {
		if i >= maxRows {
			fmt.Fprintf(&b, "…and %d more IPs\n", len(offenders)-maxRows)
			break
		}
		fmt.Fprintf(&b, "%-22s %7d %6d %6d %5d\n",
			regionLabel(s), s.Requests, s.RateLimited, s.Forbidden, s.Cooldowns)
	}
	b.WriteString("```")
	return b.String()
}

// regionLabel prefers the human region name, falling back to the endpoint
// URL when a stat somehow lacks one.
func regionLabel(s EndpointStat) string {
	if s.Region != "" {
		return s.Region
	}
	return strings.TrimPrefix(s.URL, "https://")
}
