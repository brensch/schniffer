package providers

import (
	"context"
	"fmt"
	"testing"
)

func TestMultipleCampgroundAmenities(t *testing.T) {
	provider := NewReserveCalifornia()
	ctx := context.Background()

	// Get a few campgrounds to test
	campgrounds, err := provider.FetchAllCampgrounds(ctx)
	if err != nil {
		t.Fatalf("Error fetching campgrounds: %v", err)
	}

	fmt.Printf("Testing amenities for first 5 campgrounds:\n\n")

	// Show amenities for the first few campgrounds
	for i, cg := range campgrounds {
		if i >= 5 {
			break
		}
		fmt.Printf("Campground %d: %s\n", i+1, cg.Name)
		fmt.Printf("  ID: %s\n", cg.ID)
		fmt.Printf("  Amenities: %v\n", cg.Amenities)
		fmt.Printf("  Rating: %.1f\n", cg.Rating)
		fmt.Println()

		// For the first campground, also get campsite details
		if i == 0 {
			fmt.Printf("  Getting campsite details for first campground...\n")
			campsites, err := provider.FetchCampsites(ctx, cg.ID)
			if err != nil {
				fmt.Printf("  Error fetching campsites: %v\n", err)
			} else {
				fmt.Printf("  Found %d campsites\n", len(campsites))
				if len(campsites) > 0 {
					fmt.Printf("  Sample campsite amenities: %v\n", campsites[0].Amenities)
				}
			}
			fmt.Println()
		}
	}
}
