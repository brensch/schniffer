build:
	go build -o schniffer ./cmd/schniffer

run:
	DB_PATH=./schniffer.sqlite go run ./cmd/schniffer
