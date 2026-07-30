package main

import (
	"context"
	"fmt"
	"os"

	"github.com/keys-i/blackbeard/cli/internal/app"
)

var version = "dev"

func main() {
	if err := app.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version); err != nil {
		fmt.Fprintln(os.Stderr, "blackbeard:", err)
		os.Exit(1)
	}
}
