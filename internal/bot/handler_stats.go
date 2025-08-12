package bot

import (
	"context"
	"fmt"

	"github.com/bwmarrin/discordgo"
)

func (b *Bot) handleStatsCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	active, lookups, notes, _ := b.store.StatsToday(context.Background())
	respond(s, i, fmt.Sprintf("active requests: %d\nlookups today: %d\nnotifications today: %d", active, lookups, notes))
}
