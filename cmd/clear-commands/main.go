package main

import (
	"fmt"
	"log"
	"os"

	"github.com/bwmarrin/discordgo"
)

// run this to clear the globally stored commands.
func main() {
	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN environment variable is required")
	}

	guildID := os.Getenv("GUILD_ID") // Optional - leave empty to clear global commands

	session, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal("Error creating Discord session: ", err)
	}

	err = session.Open()
	if err != nil {
		log.Fatal("Error opening connection: ", err)
	}
	defer session.Close()

	// Get application ID
	app, err := session.Application("@me")
	if err != nil {
		log.Fatal("Error getting application info: ", err)
	}

	// Clear guild commands if guild ID is provided
	if guildID != "" {
		fmt.Printf("Clearing commands for guild: %s\n", guildID)
		commands, err := session.ApplicationCommands(app.ID, guildID)
		if err != nil {
			log.Printf("Error fetching guild commands: %v\n", err)
		} else {
			for _, cmd := range commands {
				fmt.Println("guild command:", cmd.Name, cmd.ID)
				// err := session.ApplicationCommandDelete(app.ID, guildID, cmd.ID)
				// if err != nil {
				// 	log.Printf("Error deleting guild command %s: %v\n", cmd.Name, err)
				// } else {
				// 	fmt.Printf("Deleted guild command: %s\n", cmd.Name)
				// }
			}
		}
	}

	// Clear global commands
	fmt.Println("Clearing global commands...")
	commands, err := session.ApplicationCommands(app.ID, "")
	if err != nil {
		log.Printf("Error fetching global commands: %v\n", err)
	} else {
		for _, cmd := range commands {
			fmt.Println("global command:", cmd.Options, cmd.Name, cmd.ID, cmd.NSFW, cmd.ApplicationID, cmd.GuildID)
			for _, opt := range cmd.Options {
				fmt.Println("  option:", opt.Name, opt.Autocomplete, opt.Type, opt.Description)
				for _, choice := range opt.Choices {
					fmt.Println("    choice:", choice.Name, choice.Value)
				}
			}
			// err := session.ApplicationCommandDelete(app.ID, "", cmd.ID)
			// if err != nil {
			// 	log.Printf("Error deleting global command %s: %v\n", cmd.Name, err)
			// } else {
			// 	fmt.Printf("Deleted global command: %s\n", cmd.Name)
			// }
		}
	}

	fmt.Println("Command cleanup complete!")
}
