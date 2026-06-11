package bot

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/brensch/schniffer/internal/booker"
	"github.com/bwmarrin/discordgo"
)

const (
	linkModalID    = "schniff_link_modal"
	linkEmailField = "email"
	linkPassField  = "password"

	// auditURL points users at the encryption code so they can verify how
	// their password is stored before submitting it.
	auditURL = "https://github.com/brensch/schniffer/tree/main/internal/secrets"

	recGovProvider = "recreation_gov"
)

func (b *Bot) handleLinkCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if b.secrets == nil || b.pool == nil {
		respond(s, i, "auto-booking is not configured on this server")
		return
	}
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: linkModalID,
			Title:    "Link recreation.gov account",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:    linkEmailField,
						Label:       "recreation.gov email",
						Style:       discordgo.TextInputShort,
						Required:    true,
						MaxLength:   200,
						Placeholder: "you@example.com",
					},
				}},
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.TextInput{
						CustomID:    linkPassField,
						Label:       "recreation.gov password",
						Style:       discordgo.TextInputShort,
						Required:    true,
						MaxLength:   200,
						Placeholder: "password",
					},
				}},
			},
		},
	})
	if err != nil {
		b.logger.Warn("open link modal failed", slog.Any("err", err))
	}
}

func (b *Bot) handleUnlinkCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := getUserID(i)
	if userID == "" {
		respond(s, i, "could not identify user")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := b.store.DeleteUserCredential(ctx, userID); err != nil {
		b.logger.Warn("delete credential failed", slog.Any("err", err))
		respond(s, i, "failed to remove credentials: "+err.Error())
		return
	}
	if b.pool != nil {
		b.pool.StopUser(userID)
	}
	respond(s, i, "Your recreation.gov credentials have been removed.")
}

func (b *Bot) handleModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ModalSubmitData()
	if data.CustomID != linkModalID {
		return
	}
	userID := getUserID(i)
	if userID == "" {
		respond(s, i, "could not identify user")
		return
	}
	email, password := extractModalCreds(data.Components)
	email = strings.TrimSpace(email)
	if email == "" || password == "" {
		respond(s, i, "email and password are required")
		return
	}
	if b.secrets == nil || b.pool == nil {
		respond(s, i, "auto-booking is not configured on this server")
		return
	}

	// Acknowledge quickly so we don't blow Discord's 3-second window while
	// we encrypt + launch Chrome.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: 1 << 6},
	}); err != nil {
		b.logger.Warn("defer modal response failed", slog.Any("err", err))
		return
	}

	ct, err := b.secrets.Seal([]byte(password))
	if err != nil {
		b.editFollowup(s, i, "failed to encrypt password: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := b.store.UpsertUserCredential(ctx, userID, recGovProvider, email, ct); err != nil {
		b.editFollowup(s, i, "failed to save credentials: "+err.Error())
		return
	}

	// Warm the browser. On bad creds, surface the error so the user can fix
	// the typo before we lock the credential out.
	b.pool.StopUser(userID) // reset any stale session from a prior link
	if err := b.pool.StartUser(ctx, userID); err != nil {
		if errors.Is(err, booker.ErrBadCredentials) {
			_ = b.store.DisableUserCredential(ctx, userID, "initial login failed")
			b.editFollowup(s, i, "Recreation.gov rejected those credentials. Try `/schniff link` again with the correct password.")
			return
		}
		b.logger.Warn("pool start failed", slog.Any("err", err))
		b.editFollowup(s, i, "Saved your credentials but warming the browser failed: "+err.Error())
		return
	}

	// Open warm tabs for every active schniff this user already has, so
	// after linking they're immediately ready for fast-path bookings —
	// not waiting up to 30s for the reconcile loop's first tick.
	go b.kickUserWarmTabsAllActive(userID)

	b.editFollowup(s, i, linkSuccessMessage(email))
}

// kickUserWarmTabsAllActive scans the user's active schniffs and fires
// EnsureWarmTabForRequest for each, in parallel. Called right after
// /schniff link succeeds so the existing schniff set goes warm without
// waiting for the next reconcile-loop tick. Errors are logged; the
// reconcile loop will retry on its cadence.
func (b *Bot) kickUserWarmTabsAllActive(userID string) {
	if b.pool == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	reqs, err := b.store.ListUserActiveRequests(ctx, userID)
	cancel()
	if err != nil {
		b.logger.Warn("list user active requests for warm-tab kick failed",
			slog.String("user_id", userID), slog.Any("err", err))
		return
	}
	for _, r := range reqs {
		ctx, cancel := context.WithTimeout(context.Background(), warmTabRegisterTimeout)
		if err := b.pool.OpenWarmTabForRequestAsync(ctx, userID, r.ID, r.CampgroundID); err != nil {
			b.logger.Warn("post-link warm-tab slot claim failed",
				slog.String("user_id", userID),
				slog.Int64("request_id", r.ID),
				slog.String("campground_id", r.CampgroundID),
				slog.Any("err", err))
		}
		cancel()
	}
}

func extractModalCreds(rows []discordgo.MessageComponent) (email, password string) {
	for _, row := range rows {
		ar, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range ar.Components {
			ti, ok := c.(*discordgo.TextInput)
			if !ok {
				continue
			}
			switch ti.CustomID {
			case linkEmailField:
				email = ti.Value
			case linkPassField:
				password = ti.Value
			}
		}
	}
	return
}

func (b *Bot) editFollowup(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content})
	if err != nil {
		b.logger.Warn("edit followup failed", slog.Any("err", err))
	}
}

func linkSuccessMessage(email string) string {
	return strings.Join([]string{
		"✅ Linked recreation.gov account **" + email + "**.",
		"",
		"⚠️ **Important security notes:**",
		"• Do not reuse this password on any other service. Use a unique password here.",
		"• Do not save a credit card to your recreation.gov account. Remove any saved cards at https://www.recreation.gov/account/wallet.",
		"• Your password is encrypted at rest with AES-256-GCM. Audit the code: " + auditURL,
		"",
		"When schniffer finds an opening, it will hold the best campsite in your cart automatically. You finish checkout in your browser.",
	}, "\n")
}
