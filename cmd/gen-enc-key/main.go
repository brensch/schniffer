// gen-enc-key prints a fresh 32-byte AES key (base64) for SCHNIFFER_ENC_KEY.
package main

import (
	"fmt"
	"os"

	"github.com/brensch/schniffer/internal/secrets"
)

func main() {
	k, err := secrets.GenerateKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(k)
}
