//go:build darwin || linux

package torrent

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadLocalRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.torrent")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readLocal(context.Background(), path)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidSource) {
			t.Fatalf("got %v, want ErrInvalidSource", err)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO inspection blocked")
	}
}
