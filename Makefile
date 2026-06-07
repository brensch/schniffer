build:
	go build -o schniffer ./cmd/schniffer

run:
	DB_PATH=./schniffer.sqlite go run ./cmd/schniffer

gen-enc-key:
	@go run ./cmd/gen-enc-key
