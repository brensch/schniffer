build:
	go build -o schniffer ./cmd/schniffer

run:
	DUCKDB_PATH=./schniffer.duckdb go run ./cmd/schniffer
