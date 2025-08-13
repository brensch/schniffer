# Docker Deployment

This directory contains the Docker deployment configuration for the Schniffer Discord bot.

## Setup

1. **Create environment file**:
   ```bash
   cp .env.example .env
   ```

2. **Edit the .env file**:
   ```bash
   nano .env
   ```
   
   Update the following values:
   - `DISCORD_TOKEN`: Your Discord bot token
   - `GUILD_ID`: Your Discord guild (server) ID

3. **Build and run**:
   ```bash
   docker-compose up -d
   ```

## Configuration

The application reads configuration from the `.env` file:

- `DISCORD_TOKEN`: Your Discord bot token
- `GUILD_ID`: Your Discord guild/server ID  
- `DUCKDB_PATH`: Database file path (defaults to `/app/data/schniffer.duckdb`)

## Commands

- Start: `docker-compose up -d`
- Stop: `docker-compose down`
- View logs: `docker-compose logs -f schniffer`
- Restart: `docker-compose restart schniffer`

## Database

The DuckDB database is stored in the `schniffer_data` Docker volume at `/app/data/schniffer.duckdb` inside the container.
