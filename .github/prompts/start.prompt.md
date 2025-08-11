---
mode: agent
---
ok big dawg show me your full power. write me an application that:

- takes inputs of dates for a given campground
- records the user that made the request and the request. requests should include the ground and the time range they're looking for
- once every 5 seconds it should check for any active requests (schniffs) and make a request for each campground being monitored (only one lookup per campground, not per request, ie multiple users looking for the same ground over the same time range should not cause multiple lookups)
- it should record the state of all sites on the campground and if any go from unavailable to available it should send the user a notification
- it should notify them again when that campsite becomes unavailable again
- state should be tracked in a file backed duckdb instance
- logic for recreation.gov should be checked from this repo: https://github.com/brensch/campbot. however it should be set up so that other campground state providers can be added
- it should be written in go
- campground state providers should be abstracted behind a common interface
- all interactions with the application should be done via discord
- it should have a way to look up stats from the duckdb instance and write a summary once a day
- all relevant things should be recorded to the duckdb instance so that i can make a grafana dashboard to show what it's doing.

