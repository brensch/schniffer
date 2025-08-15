package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/brensch/schniffer/internal/bot"
	"github.com/brensch/schniffer/internal/db"
	"github.com/brensch/schniffer/internal/manager"
	"github.com/brensch/schniffer/internal/providers"
	"github.com/brensch/schniffer/internal/web"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	// Background campground sync (daily)
	go mgr.RunCampgroundSync(ctx, "recreation_gov", 24*60*60*1e9)
	go mgr.RunCampgroundSync(ctx, "reservecalifornia", 24*60*60*1e9)

	// Start web server
	webAddr := os.Getenv("WEB_ADDR")
	if webAddr == "" {
		webAddr = ":8069"
	}
	webServer := web.NewServer(store, webAddr)
	go func() {
		if err := webServer.Run(ctx); err != nil {
			slog.Error("web server failed", slog.Any("err", err))
		}
	}()

	prod := os.Getenv("PROD") == "true"
	if discordToken == "" {
		slog.Error("DISCORD_TOKEN env var required")
		os.Exit(1)
	}

	b := bot.New(discordToken, guildID, store, mgr, !prod)
	if err := b.Run(ctx); err != nil {
		slog.Error("bot run failed", slog.Any("err", err))
		os.Exit(1)
	}
}
