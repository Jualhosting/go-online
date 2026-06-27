//go:build client

package main

import (
	"fmt"
	"os"
)

func runServer() {
	fmt.Println("Error: This binary is compiled in client-only mode. Server subcommand is unavailable.")
	os.Exit(1)
}
