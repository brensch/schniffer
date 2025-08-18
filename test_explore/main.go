package main

import (
	"context"
	"fmt"
	"log"

	"github.com/brensch/schniffer/internal/providers"
)

func main() {
	// Create ReserveCalifornia provider
	provider := providers.NewReserveCalifornia()

	// Test with a known campground - let's try Hearst San Simeon SP
	// Based on the examples, this has PlaceId=713 with facilities
	campgroundID := "713/789" // PlaceId/FacilityId format

	fmt.Printf("Exploring amenities for campground: %s\n\n", campgroundID)

	// First, let's see what campsites we get
	ctx := context.Background()
	campsites, err := provider.FetchCampsites(ctx, campgroundID)
	if err != nil {
		log.Fatalf("Error fetching campsites: %v", err)
	}

	fmt.Printf("Found %d campsites\n\n", len(campsites))

	// Show details for first few campsites
	for i, site := range campsites {
		if i >= 3 { // Only show first 3 for brevity
			break
		}
		fmt.Printf("Campsite %d:\n", i+1)
		fmt.Printf("  ID: %s\n", site.ID)
		fmt.Printf("  Name: %s\n", site.Name)
		fmt.Printf("  Type: %s\n", site.Type)
		fmt.Printf("  Equipment: %v\n", site.Equipment)
		fmt.Printf("  Cost/Night: $%.2f\n", site.CostPerNight)
		fmt.Printf("  Image: %s\n", site.PreviewImageURL)
		fmt.Println()
	}

	// Now let's also check campground-level amenities
	campgrounds, err := provider.FetchAllCampgrounds(ctx)
	if err != nil {
		log.Fatalf("Error fetching campgrounds: %v", err)
	}

	fmt.Printf("Found %d total campgrounds\n\n", len(campgrounds))

	// Find our test campground and show its amenities
	for _, cg := range campgrounds {
		if cg.ID == campgroundID {
			fmt.Printf("Campground: %s\n", cg.Name)
			fmt.Printf("Amenities: %v\n", cg.Amenities)
			break
		}
	}
}
