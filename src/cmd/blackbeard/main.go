package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"

	"github.com/keys-i/blackbeard/src/internal/app"
	"github.com/keys-i/blackbeard/src/internal/termtext"
	"github.com/keys-i/blackbeard/src/internal/version"
)

func main() {
	ctx, stop := commandContext()
	err := app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version.Version)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "blackbeard:", termtext.Sanitize(err.Error(), 4096))
		os.Exit(app.ExitCode(err))
	}
}

func commandContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(interrupts)
			cancel()
		})
	}
	go func() {
		select {
		case <-interrupts:
			stop()
		case <-ctx.Done():
		}
	}()
	return ctx, stop
}
