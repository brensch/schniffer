# Build stage
FROM golang:1.24-alpine AS builder

# Install build dependencies
RUN apk add --no-cache gcc musl-dev

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=1 GOOS=linux go build -a -installsuffix cgo -o schniffer ./cmd/schniffer

# Runtime stage
FROM alpine:latest

# Install runtime dependencies
RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy the binary from builder stage
COPY --from=builder /app/schniffer .

# Copy database schema if needed
COPY --from=builder /app/internal/db/schema.sql ./internal/db/

# Create directory for database
RUN mkdir -p /app/data

# Set environment variable for database path
ENV DUCKDB_PATH=/app/data/schniffer.duckdb

# Expose no ports since this is a Discord bot (no HTTP server)

# Run the application
CMD ["./schniffer"]
