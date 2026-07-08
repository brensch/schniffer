package bot

import (
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
)

// handleDashboardCommand mints a single-use, expiring link to the private
// monitoring dashboard — but only for the configured admin. Everyone else
// gets a refusal and no token. The reply is always ephemeral so the link is
// visible only to the person who ran the command.
func (b *Bot) handleDashboardCommand(s *discordgo.Session, i *discordgo.InteractionCreate, _ *discordgo.ApplicationCommandInteractionDataOption) {
	if b.dashAuth == nil || b.dashAdminID == "" || b.dashBaseURL == "" {
		respond(s, i, "The dashboard isn't configured on this instance.")
		return
	}
	if getUserID(i) != b.dashAdminID {
		// Do not reveal that a dashboard exists in any actionable way, and
		// never mint a token for a non-admin.
		respond(s, i, "Sorry — the monitoring dashboard is restricted to the admin.")
		return
	}

	token, err := b.dashAuth.MintToken()
	if err != nil {
		b.logger.Error("dashboard token mint failed", slog.Any("err", err))
		respond(s, i, "Couldn't create a dashboard link right now. Try again.")
		return
	}

	url := fmt.Sprintf("%s/monitor?token=%s", b.dashBaseURL, token)
	respond(s, i, fmt.Sprintf(
		"🖥️ **Live dashboard** — this link is for you only and expires in 5 minutes (one use):\n%s",
		url,
	))
}
