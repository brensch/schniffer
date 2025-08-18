package main

import (
	"context"
	"fmt"
	"log"

	"github.com/brensch/schniffer/internal/db"
)

func main() {
	// Test the GetCampgroundByID function
	store, err := db.Open("./schniffer.sqlite")
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()

	// Test with a known campground
	campground, exists, err := store.GetCampgroundByID(context.Background(), "recreation_gov", "274410")
	if err != nil {
		log.Printf("Error: %v", err)
		return
	}

	if !exists {
		log.Println("Campground not found")
		return
	}

	fmt.Printf("Found campground: %+v\n", campground)

	// Test the new campsite details function
	details, err := store.GetCampsiteDetails(context.Background(), "recreation_gov", "10361324", "10361328")
	if err != nil {
		log.Printf("Error getting campsite details: %v", err)
	} else {
		fmt.Printf("Campsite details: %+v\n", details)
	}

	// Test batch campsite details
	batchDetails, err := store.GetCampsiteDetailsBatch(context.Background(), "recreation_gov", "10361324", []string{"10361328", "10361330"})
	if err != nil {
		log.Printf("Error getting batch campsite details: %v", err)
	} else {
		fmt.Printf("Batch campsite details: %+v\n", batchDetails)
	}
}
