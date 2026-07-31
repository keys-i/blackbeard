package main

import (
	"context"
	"fmt"
	"os"

	"github.com/keys-i/blackbeard/src/internal/app"
	"github.com/keys-i/blackbeard/src/internal/version"
)

func main() {
	if err := app.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version.Version); err != nil {
		fmt.Fprintln(os.Stderr, "blackbeard:", err)
		os.Exit(1)
	}
}
