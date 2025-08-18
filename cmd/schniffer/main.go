package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/brensch/schniffer/internal/bot"
	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/manager"
	"github.com/brensch/schniffer/internal/providers"
	"github.com/brensch/schniffer/internal/web"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Simple CLI handling for sync commands
	if len(os.Args) >= 2 && os.Args[1] == "sync" {
		handleSyncCommand(ctx)
		return
	}

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

	discordToken := os.Getenv("DISCORD_TOKEN")
	guildID := os.Getenv("GUILD_ID")

	mgr := manager.NewManager(store, provRegistry)
	mgr.SetSummaryChannel(guildID) // Use guild ID as the summary channel ID
	go mgr.Run(ctx)
	go mgr.RunDailySummary(ctx)

	// Background campground sync (weekly)
	syncFrequency := 7 * 24 * time.Hour
	go mgr.RunCampgroundSync(ctx, "recreation_gov", syncFrequency)
	go mgr.RunCampgroundSync(ctx, "reservecalifornia", syncFrequency)

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

	prod := os.Getenv("PROD") == "true"
	if discordToken == "" {
		slog.Error("DISCORD_TOKEN env var required")
		os.Exit(1)
	}

	b := bot.New(discordToken, guildID, store, mgr, !prod)
	err = b.Run(ctx)
	if err != nil {
		slog.Error("bot run failed", slog.Any("err", err))
		os.Exit(1)
	}
}

func handleSyncCommand(ctx context.Context) {
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

	mgr := manager.NewManager(store, provRegistry)

	// Parse simple command line args
	provider := ""
	syncType := ""
	for i, arg := range os.Args {
		if arg == "--provider" && i+1 < len(os.Args) {
			provider = os.Args[i+1]
		}
		if arg == "--type" && i+1 < len(os.Args) {
			syncType = os.Args[i+1]
		}
	}

	if provider == "" {
		slog.Error("--provider required")
		os.Exit(1)
	}

	if syncType == "" {
		slog.Error("--type required (campgrounds or campsites)")
		os.Exit(1)
	}

	switch syncType {
	case "campgrounds":
		// First sync campgrounds
		count, err := mgr.SyncCampgrounds(ctx, provider)
		if err != nil {
			slog.Error("campground sync failed", slog.String("provider", provider), slog.Any("err", err))
			os.Exit(1)
		}
		slog.Info("campground sync completed", slog.String("provider", provider), slog.Int("count", count))

	case "campsites":
		count, err := mgr.SyncCampsites(ctx, provider)
		if err != nil {
			slog.Error("campsite sync failed", slog.String("provider", provider), slog.Any("err", err))
			os.Exit(1)
		}
		slog.Info("campsite sync completed", slog.String("provider", provider), slog.Int("count", count))

	default:
		slog.Error("invalid sync type", slog.String("type", syncType))
		os.Exit(1)
	}
}
