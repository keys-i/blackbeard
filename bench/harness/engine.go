// Command enginebench is a development-only loopback torrent harness.
//
// Copy this file into a temporary Go module before running it. Keeping the
// harness outside src avoids adding the selected engine to the product graph
// before the hardened adapter lands.
package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

type options struct {
	mode       string
	transport  string
	data       string
	torrent    string
	payload    string
	peer       string
	seed       string
	port       int
	bytes      int64
	pieceBytes int64
}

type result struct {
	Bytes      int64 `json:"bytes"`
	ElapsedNS  int64 `json:"elapsed_ns"`
	Goroutines int   `json:"goroutines"`
	Peers      int   `json:"peers"`
}

func main() {
	var opts options
	flag.StringVar(&opts.mode, "mode", "", "fixture, seed, or fetch")
	flag.StringVar(&opts.transport, "transport", "tcp", "tcp or utp")
	flag.StringVar(&opts.data, "data", "", "seed or destination directory")
	flag.StringVar(&opts.torrent, "torrent", "", "metainfo file")
	flag.StringVar(&opts.payload, "payload", "", "fixture payload file")
	flag.StringVar(&opts.peer, "peer", "", "fixed loopback peer")
	flag.StringVar(&opts.seed, "seed", "blackbeard-engine-bakeoff-v1", "fixture seed")
	flag.IntVar(&opts.port, "port", 0, "listen port")
	flag.Int64Var(&opts.bytes, "bytes", 512<<20, "fixture payload bytes")
	flag.Int64Var(&opts.pieceBytes, "piece-bytes", 1<<20, "fixture piece bytes")
	flag.Parse()

	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(opts options) error {
	switch opts.mode {
	case "fixture":
		return createFixture(opts)
	case "seed", "fetch":
		return transfer(opts)
	default:
		return fmt.Errorf("invalid mode %q", opts.mode)
	}
}

func createFixture(opts options) error {
	if opts.payload == "" || opts.torrent == "" {
		return errors.New("fixture requires -payload and -torrent")
	}
	if opts.bytes <= 0 || opts.pieceBytes <= 0 {
		return errors.New("fixture sizes must be positive")
	}
	payload, err := os.OpenFile(opts.payload, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create payload: %w", err)
	}
	complete := false
	defer func() {
		_ = payload.Close()
		if !complete {
			_ = os.Remove(opts.payload)
		}
	}()

	key := sha256.Sum256([]byte(opts.seed))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return fmt.Errorf("create deterministic stream: %w", err)
	}
	stream := cipher.StreamReader{S: cipher.NewCTR(block, make([]byte, aes.BlockSize)), R: zeroReader{}}
	if _, err := io.CopyN(payload, stream, opts.bytes); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	if err := payload.Sync(); err != nil {
		return fmt.Errorf("sync payload: %w", err)
	}
	if err := payload.Close(); err != nil {
		return fmt.Errorf("close payload: %w", err)
	}

	info := metainfo.Info{PieceLength: opts.pieceBytes}
	if err := info.BuildFromFilePath(opts.payload); err != nil {
		return fmt.Errorf("hash payload: %w", err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		return fmt.Errorf("encode info dictionary: %w", err)
	}
	out, err := os.OpenFile(opts.torrent, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create metainfo: %w", err)
	}
	if err := (&metainfo.MetaInfo{
		InfoBytes: infoBytes,
		CreatedBy: "blackbeard engine bake-off",
	}).Write(out); err != nil {
		_ = out.Close()
		_ = os.Remove(opts.torrent)
		return fmt.Errorf("write metainfo: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(opts.torrent)
		return fmt.Errorf("close metainfo: %w", err)
	}
	complete = true
	return nil
}

func transfer(opts options) error {
	if opts.transport != "tcp" && opts.transport != "utp" {
		return fmt.Errorf("invalid transport %q", opts.transport)
	}
	meta, err := metainfo.LoadFromFile(opts.torrent)
	if err != nil {
		return fmt.Errorf("load metainfo: %w", err)
	}
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = opts.data
	cfg.ListenHost = torrent.LoopbackListenHost
	cfg.ListenPort = opts.port
	cfg.NoDHT = true
	cfg.DisableTrackers = true
	cfg.DisablePEX = true
	cfg.DisableWebtorrent = true
	cfg.DisableWebseeds = true
	cfg.NoDefaultPortForwarding = true
	cfg.DisableIPv6 = true
	cfg.DisableTCP = opts.transport != "tcp"
	cfg.DisableUTP = opts.transport != "utp"
	cfg.Seed = opts.mode == "seed"
	cfg.Slogger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	started := time.Now()
	client, err := torrent.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("start client: %w", err)
	}
	defer client.Close()
	spec := torrent.TorrentSpecFromMetaInfo(meta)
	spec.IgnoreUnverifiedPieceCompletion = true
	active, _, err := client.AddTorrentSpec(spec)
	if err != nil {
		return fmt.Errorf("add torrent: %w", err)
	}
	if opts.mode == "seed" {
		return seed(ctx, active)
	}
	if opts.peer == "" {
		return errors.New("fetch requires -peer")
	}
	if active.AddPeers([]torrent.PeerInfo{{Addr: torrent.StringAddr(opts.peer)}}) != 1 {
		return errors.New("fixed peer was not added")
	}
	active.DownloadAll()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-active.Complete().On():
	}
	return json.NewEncoder(os.Stdout).Encode(result{
		Bytes:      active.BytesCompleted(),
		ElapsedNS:  time.Since(started).Nanoseconds(),
		Goroutines: runtime.NumGoroutine(),
		Peers:      active.Stats().ActivePeers,
	})
}

func seed(ctx context.Context, active *torrent.Torrent) error {
	if err := active.VerifyData(); err != nil {
		return fmt.Errorf("verify seed: %w", err)
	}
	select {
	case <-active.Complete().On():
	case <-time.After(10 * time.Second):
		return errors.New("seed data failed verification")
	case <-ctx.Done():
		return ctx.Err()
	}
	fmt.Println("ready")
	<-ctx.Done()
	return nil
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
