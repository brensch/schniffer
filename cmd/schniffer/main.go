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
