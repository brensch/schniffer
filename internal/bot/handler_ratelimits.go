package bot

import (
	"time"

	"github.com/brensch/schniffer/internal/httpx"
	"github.com/brensch/schniffer/internal/proxypool"
	"github.com/bwmarrin/discordgo"
)

// handleRateLimitsCommand answers /schniff ratelimits with a live per-IP
// rate-limit breakdown, covering traffic since the last nightly report
// reset. Ephemeral (respond sets the ephemeral flag) so anyone can check
// without cluttering the channel.
func (b *Bot) handleRateLimitsCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	pool := httpx.Pool()
	if pool == nil {
		respond(s, i, "The proxy pool is disabled, so there are no per-IP rate-limit stats to show.")
		return
	}
	stats, since := pool.Snapshot()
	respond(s, i, proxypool.FormatReport(stats, since, time.Now()))
}
