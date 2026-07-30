package app

import (
	"context"
	"fmt"
	"io"
)

const usage = `Blackbeard — chart lawful torrents from the terminal

Usage:
  blackbeard
  blackbeard search QUERY
  blackbeard inspect MAGNET_OR_TORRENT
  blackbeard fetch MAGNET_OR_TORRENT
  blackbeard config
  blackbeard version
  blackbeard help
`

func Run(_ context.Context, args []string, _ io.Reader, stdout, _ io.Writer, version string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	if args[0] == "version" || args[0] == "--version" {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}
	return fmt.Errorf("%q is not implemented yet", args[0])
}
