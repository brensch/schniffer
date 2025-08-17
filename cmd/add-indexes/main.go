package main

import (
	"log"

	"github.com/brensch/schniffer/internal/db"
)

func main() {
	store, err := db.Open("schniffer.sqlite")
	if err != nil {
		log.Fatal("Failed to open store:", err)
	}
	defer store.Close()

	log.Println("Adding performance indexes...")

	// Add indexes for campground_types table
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_campground_types_lookup ON campground_types(provider, campground_id)`,
		`CREATE INDEX IF NOT EXISTS idx_campground_types_composite ON campground_types(provider, campground_id, campsite_type)`,
	}

	for _, indexSQL := range indexes {
		_, err = store.DB.Exec(indexSQL)
		if err != nil {
			log.Printf("Warning: Failed to create index: %v", err)
		} else {
			log.Printf("Created index: %s", indexSQL)
		}
	}

	log.Println("Index creation complete")
}
