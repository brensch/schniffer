package bot

import (
	"fmt"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleStatsCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	row := b.store.DB.QueryRow(`
        SELECT coalesce((SELECT count(*) FROM schniff_requests WHERE active=true),0),
        coalesce((SELECT count(*) FROM lookup_log WHERE date(checked_at)=current_date),0),
        coalesce((SELECT count(*) FROM notifications WHERE date(sent_at)=current_date),0)
    `)
	var active, lookups, notes int64
	_ = row.Scan(&active, &lookups, &notes)
	respond(s, i, fmt.Sprintf("active requests: %d\nlookups today: %d\nnotifications today: %d", active, lookups, notes))
}
