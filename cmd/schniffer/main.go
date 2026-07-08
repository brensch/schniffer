package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	// Embed the full IANA timezone database into the binary so
	// time.LoadLocation works regardless of whether /usr/share/zoneinfo
	// exists in the container. Costs ~450 KB at build time.
	_ "time/tzdata"

	"github.com/brensch/schniffer/internal/booker"
	"github.com/brensch/schniffer/internal/bot"
	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/manager"
	"github.com/brensch/schniffer/internal/metrics"
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

	// Start the metrics server first so /metrics is up even if a later
	// dependency (Discord, DB) blows up — makes debug-by-scrape possible
	// during startup failures. Bind synchronously so the port is ready
	// before we return; serve in the background.
	metricsAddr := os.Getenv("METRICS_ADDR")
	if metricsAddr == "" {
		metricsAddr = ":9090"
	}
	if err := startMetricsServer(ctx, metricsAddr); err != nil {
		slog.Error("metrics server failed to bind", slog.Any("err", err))
	}

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

	// Operational alerts (rate-limit backoffs) DM this user instead of
	// pinging the summary channel. Unset → alerts go to the channel.
	if adminID := os.Getenv("ADMIN_USER_ID"); adminID != "" {
		mgr.SetAdminUser(adminID)
	}

	// Optional auto-booking: only enabled when SCHNIFFER_ENC_KEY is set.
	// Without it we can't decrypt stored passwords, so the pool stays nil and
	// notifications fall back to plain DMs.
	pool := initAutoBooking(ctx, store, discordSession, b, mgr)
	if pool != nil {
		go pool.RunRefreshLoop(ctx)
		go pool.RunWatchdog(ctx)
		defer pool.Close()
	}

	go mgr.Run(ctx)
	go mgr.RunDailySummary(ctx)
	go runHealthcheckPinger(ctx, "https://hc-ping.com/ec9f9824-6317-4fb8-a5e3-dcc9e06431d5", 10*time.Minute)

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

// startMetricsServer binds /metrics on addr synchronously and serves it in
// a background goroutine. Returns immediately once the listener is bound —
// or an error if the bind itself failed. We bind early in main() so a
// later startup panic (Discord auth, etc.) still leaves /metrics
// reachable for debugging.
func startMetricsServer(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	slog.Info("starting metrics server", slog.String("addr", ln.Addr().String()))
	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server failed", slog.Any("err", err))
		}
	}()
	return nil
}

// runHealthcheckPinger fires a GET at url every interval to signal liveness to
// an external watchdog (e.g. healthchecks.io). Pings immediately on start so a
// fresh deploy is reflected without waiting a full interval.
func runHealthcheckPinger(ctx context.Context, url string, interval time.Duration) {
	client := &http.Client{Timeout: 10 * time.Second}
	ping := func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			slog.Warn("healthcheck request build failed", slog.Any("err", err))
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("healthcheck ping failed", slog.Any("err", err))
			return
		}
		_ = resp.Body.Close()
	}
	ping()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ping()
		}
	}
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
