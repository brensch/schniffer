package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/brensch/schniffer/internal/booker"
	"github.com/brensch/schniffer/internal/bot"
	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/manager"
	"github.com/brensch/schniffer/internal/providers"
	"github.com/brensch/schniffer/internal/secrets"
	"github.com/brensch/schniffer/internal/web"
	"github.com/bwmarrin/discordgo"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// set log level to debug
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})))

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "./schniffer.sqlite"
	}

	store, err := db.Open(dbPath)
	if err != nil {
		slog.Error("open db failed", slog.Any("err", err))
		os.Exit(1)
	}
	defer store.Close()

	provRegistry := providers.NewRegistry()
	provRegistry.Register("recreation_gov", providers.NewRecreationGov())
	provRegistry.Register("reservecalifornia", providers.NewReserveCalifornia())

	// both manager and bot use this so shared
	discordSession, err := discordgo.New("Bot " + os.Getenv("DISCORD_TOKEN"))
	if err != nil {
		panic(err)
	}
	// must register intents before opening
	discordSession.Identify.Intents =
		discordgo.IntentsGuilds |
			discordgo.IntentsGuildMessages |
			discordgo.IntentDirectMessages |
			discordgo.IntentsGuildMembers

	prod := os.Getenv("PROD") == "true"
	guildID := os.Getenv("GUILD_ID")
	broadcastChannel, err := bot.GuildIDToChannelID(discordSession, guildID)
	if err != nil {
		panic(err)
	}

	b, err := bot.New(store, discordSession, provRegistry, guildID, !prod)
	if err != nil {
		slog.Error("failed to create bot", slog.Any("err", err))
		panic(err)
	}
	err = b.MountHandlers()
	if err != nil {
		slog.Error("bot mount handlers failed", slog.Any("err", err))
		panic(err)
	}

	err = discordSession.Open()
	if err != nil {
		panic(err)
	}
	defer discordSession.Close()

	mgr := manager.NewManager(store, provRegistry, discordSession, broadcastChannel)

	// Optional auto-booking: only enabled when SCHNIFFER_ENC_KEY is set.
	// Without it we can't decrypt stored passwords, so the pool stays nil and
	// notifications fall back to plain DMs.
	pool := initAutoBooking(ctx, store, discordSession, b, mgr)
	if pool != nil {
		go pool.RunRefreshLoop(ctx)
		defer pool.Close()
	}

	go mgr.Run(ctx)
	go mgr.RunDailySummary(ctx)

	// // Background metadata sync
	// go mgr.RunCampgroundSync(ctx, "recreation_gov")
	// go mgr.RunCampgroundSync(ctx, "reservecalifornia")

	// Start web server
	webAddr := os.Getenv("WEB_ADDR")
	if webAddr == "" {
		webAddr = ":8069"
	}
	webServer := web.NewServer(store, mgr, webAddr)
	go func() {
		err := webServer.Run(ctx)
		if err != nil {
			slog.Error("web server failed", slog.Any("err", err))
		}
	}()

	<-ctx.Done()
	slog.Info("night night")
}

// initAutoBooking wires up the secrets box, browser pool, and bot/manager
// integrations. Returns nil if SCHNIFFER_ENC_KEY is unset — the bot still
// runs, just without auto-booking.
func initAutoBooking(
	ctx context.Context,
	store *db.Store,
	session *discordgo.Session,
	b *bot.Bot,
	mgr *manager.Manager,
) *booker.Pool {
	if os.Getenv(secrets.EnvKey) == "" {
		slog.Info("auto-booking disabled (no SCHNIFFER_ENC_KEY)")
		return nil
	}
	key, err := secrets.LoadKeyFromEnv()
	if err != nil {
		slog.Error("load enc key failed", slog.Any("err", err))
		return nil
	}
	box, err := secrets.New(key)
	if err != nil {
		slog.Error("init secrets box failed", slog.Any("err", err))
		return nil
	}

	baseProfile := os.Getenv("BOOKER_PROFILE_DIR")
	if baseProfile == "" {
		baseProfile = filepath.Join(".cache", "recgov-profiles")
	}
	if err := os.MkdirAll(baseProfile, 0o700); err != nil {
		slog.Error("mkdir profiles base failed", slog.Any("err", err))
		return nil
	}

	lookup := func(ctx context.Context, userID string) (string, string, error) {
		cred, err := store.GetUserCredential(ctx, userID)
		if err != nil {
			return "", "", err
		}
		if cred == nil || cred.DisabledAt != nil {
			return "", "", nil
		}
		pw, err := box.Open(cred.PasswordCT)
		if err != nil {
			return "", "", fmt.Errorf("decrypt: %w", err)
		}
		return cred.Email, string(pw), nil
	}

	onDisable := func(ctx context.Context, userID, reason string) {
		if err := store.DisableUserCredential(ctx, userID, reason); err != nil {
			slog.Warn("disable credential failed", slog.String("user", userID), slog.Any("err", err))
		}
		ch, err := session.UserChannelCreate(userID)
		if err != nil {
			slog.Warn("dm channel create failed", slog.String("user", userID), slog.Any("err", err))
			return
		}
		msg := "⚠️ Your saved recreation.gov credentials no longer work (" + reason + "). " +
			"Auto-booking is paused for you. Run `/schniff link` to fix it."
		if _, err := session.ChannelMessageSend(ch.ID, msg); err != nil {
			slog.Warn("dm send failed", slog.String("user", userID), slog.Any("err", err))
		}
	}

	pool := booker.NewPool(booker.PoolConfig{
		BaseProfileDir:   baseProfile,
		LookupCredential: lookup,
		OnDisable:        onDisable,
		Logger:           slog.Default(),
	})

	b.SetAutoBooking(box, pool)
	mgr.SetAutoBooking(pool)

	// Warm every linked user's Chrome in the background; do not block startup.
	go func() {
		creds, err := store.ListActiveUserCredentials(ctx)
		if err != nil {
			slog.Warn("list credentials failed", slog.Any("err", err))
			return
		}
		ids := make([]string, 0, len(creds))
		for _, c := range creds {
			ids = append(ids, c.UserID)
		}
		slog.Info("warming browser pool", slog.Int("users", len(ids)))
		pool.StartAll(ctx, ids)
		slog.Info("browser pool warm", slog.Int("users", len(ids)))
	}()
	return pool
}
