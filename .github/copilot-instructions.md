# Schniffer Discord Bot Development Guide

**ALWAYS follow these instructions first.** Only search or use bash commands when you encounter unexpected information that conflicts with this guide or when this information is incomplete.

A Go Discord bot that monitors campground availability, records activity to SQLite/DuckDB, and notifies users when campsites become available. Includes a web interface with interactive map for campground management.

## Critical Build & Runtime Information

### Prerequisites
- **Go 1.24+ REQUIRED** - The project uses Go 1.24 features and toolchain
- **SQLite support** - Uses CGO for SQLite integration (mattn/go-sqlite3)
- **Network access** - Connects to Discord API and campground provider APIs

### Environment Variables
- `DISCORD_TOKEN` - Discord bot token (REQUIRED for bot functionality)
- `DB_PATH` - SQLite database path (defaults to `./schniffer.sqlite`)
- `GUILD_ID` - Discord guild ID (optional, for faster command registration)
- `SUMMARY_CHANNEL_ID` - Discord channel for daily summaries (optional)
- `WEB_ADDR` - Web server address (defaults to `:8069`)
- `PROD` - Set to "true" for production mode

## Build & Test Commands

### Building
```bash
# Main application - NEVER CANCEL: Takes ~45 seconds. Set timeout to 120+ seconds.
go build ./cmd/schniffer

# Using Makefile (creates binary named 'schniffer')
make build

# Clear commands utility
go build ./cmd/clear-commands
```

### Testing
```bash
# Run all tests - NEVER CANCEL: May take 5+ minutes due to network timeouts. Set timeout to 600+ seconds.
go test ./...

# Run specific test packages (faster, no network dependencies)
go test ./internal/db
go test ./internal/manager

# Note: Some tests currently fail - this is EXPECTED in current codebase state
# Tests in ./internal/providers may timeout due to external API calls
```

### Linting & Formatting
```bash
# Standard Go formatting (always run before committing)
go fmt ./...

# Static analysis
go vet ./...

# Dependency management
go mod tidy
```

## Running the Application

### Prerequisites for Running
**CRITICAL**: The bot requires a valid Discord token and will panic without it. The application immediately connects to Discord on startup.

### Local Development Run
```bash
# Set required environment variables
export DISCORD_TOKEN="your_bot_token_here"
export DB_PATH="./schniffer.sqlite"

# Run the application
go run ./cmd/schniffer

# OR using the built binary
./schniffer
```

### Web Interface
- Access at `http://localhost:8069` (or custom WEB_ADDR)
- Interactive map for campground management and group creation
- API endpoints for campground data and availability

## Docker Deployment

### Docker Build
```bash
# NEVER CANCEL: Docker build takes 10+ minutes. Set timeout to 900+ seconds.
docker build -t schniffer .
```

### Docker Compose
```bash
# Setup environment file
cp .env.example .env
# Edit .env with your Discord token and guild ID

# Run with docker-compose
docker-compose up -d

# View logs
docker-compose logs -f schniffer
```

## Validation & Testing Scenarios

### Build Validation
1. **ALWAYS test build first**: Run `go build ./cmd/schniffer` and verify it completes in ~45 seconds
2. **Test utilities**: Run `go build ./cmd/clear-commands`
3. **Dependency check**: Run `go mod tidy` to ensure clean dependencies

### Functional Validation
Since the bot requires Discord credentials, validation focuses on:

1. **Code compilation**: All packages must build without errors
2. **Static analysis**: `go vet ./...` must pass
3. **Database functionality**: Run `go test ./internal/db` for core storage tests
4. **Web interface**: Check static files in `./static/` directory are intact

### Manual Testing (with Discord credentials)
If you have valid Discord credentials:
1. Start the bot with `DISCORD_TOKEN=... go run ./cmd/schniffer`
2. Verify Discord connection (check console output)
3. Test web interface at `http://localhost:8069`
4. Test slash commands in Discord: `/schniff add`, `/schniff list`

## Project Structure & Key Components

### Main Applications
- `cmd/schniffer/` - Main Discord bot application
- `cmd/clear-commands/` - Utility to clear Discord slash commands

### Core Packages
- `internal/bot/` - Discord bot handlers and command registration
- `internal/db/` - SQLite database operations and schema
- `internal/manager/` - Campground monitoring and notification logic
- `internal/providers/` - Campground data providers (recreation.gov, reservecalifornia)
- `internal/web/` - HTTP server for web interface and API

### Static Assets
- `static/` - Web interface files (HTML, CSS, JS)
  - Interactive map using Leaflet.js
  - Campground visualization and group management

### Configuration
- `go.mod` - Go 1.24+ required, key dependencies: discordgo, sqlite3
- `Dockerfile` - Multi-stage build for production deployment
- `docker-compose.yml` - Complete deployment configuration

## Common Development Tasks

### Adding New Commands
1. Add command definition in `internal/bot/bot.go` (`registerCommands`)
2. Create handler in `internal/bot/handler_*.go`
3. Always test with `go build` and `go vet`

### Modifying Database Schema
1. Update schema in `internal/db/`
2. Run database tests: `go test ./internal/db`
3. Test migration logic carefully

### Updating Web Interface
1. Modify files in `static/` directory
2. No build step required for static files
3. Test by accessing `http://localhost:8069`

### Working with Provider APIs
1. Provider implementations in `internal/providers/`
2. **WARNING**: Provider tests make real API calls and may timeout
3. Use `go test ./internal/providers -short` to skip integration tests

## Known Issues & Limitations

### Current Test Status
- Some tests in `internal/db` and `internal/manager` currently fail
- Provider tests may timeout due to external API dependencies
- **This is expected** - focus on new code not breaking existing functionality

### Network Dependencies
- Bot requires Discord API access
- Provider tests need recreation.gov and reservecalifornia APIs
- Docker builds may fail in restricted network environments

### Build Requirements
- **CGO_ENABLED=1** required for SQLite support
- Cross-compilation needs appropriate CGO setup
- Ubuntu 24.04+ recommended for Docker builds

## Troubleshooting

### Build Failures
- Ensure Go 1.24+ is installed: `go version`
- Check CGO availability: `go env CGO_ENABLED`
- Verify dependencies: `go mod tidy && go mod verify`

### Runtime Issues
- Missing DISCORD_TOKEN causes immediate panic
- Database permission issues: Check DB_PATH directory permissions
- Port conflicts: Change WEB_ADDR if 8069 is in use

### Test Failures
- Network-dependent tests may fail in CI environments
- Focus on unit tests that don't require external APIs
- Use `-short` flag to skip integration tests: `go test -short ./...`

**Remember**: NEVER CANCEL long-running builds or tests. They may take 5-15 minutes to complete.