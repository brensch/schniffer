package main

import (
	"context"
	"log"

	"github.com/brensch/schniffer/internal/db"
)

func main() {
	store, err := db.Open("schniffer.sqlite")
	if err != nil {
		log.Fatal("Failed to create store:", err)
	}
	defer store.Close()

	log.Println("Refreshing campground types table...")

	err = store.RefreshCampgroundTypes(context.Background())
	if err != nil {
		log.Fatal("Failed to refresh campground types:", err)
	}

	log.Println("Successfully refreshed campground types table")
}
