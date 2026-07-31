# Engine bake-off harness

This development-only harness creates deterministic v1 fixtures and measures
one fixed loopback peer. It disables trackers, DHT, PEX, webseeds, WebTorrent,
IPv6, and port mapping so discovery noise cannot change the result.

Use a temporary module; do not add research dependencies to the product before
the hardened engine adapter lands:

```sh
tmp="$(mktemp -d)"
cp bench/harness/engine.go "$tmp/main.go"
cd "$tmp"
go mod init blackbeard.enginebench
go get github.com/anacrolix/torrent@v1.61.0
go mod tidy

go run . -mode fixture \
  -payload payload.bin \
  -torrent payload.torrent \
  -bytes 536870912 \
  -piece-bytes 1048576 \
  -seed blackbeard-engine-bakeoff-v1
```

Build the same source for each Go arm:

```sh
CGO_ENABLED=1 go build -trimpath -o go-native .
CGO_ENABLED=1 go build -trimpath -tags=disable_libutp -o go-pure-isolated .
CGO_ENABLED=0 go build -trimpath -o go-pure-portable .
```

For each sample, start a fresh seeder with a fixed loopback port and a fresh
destination, then measure the fetch process with the host's process accounting
tool. Example on macOS:

```sh
./go-pure-portable -mode seed -transport tcp \
  -data seed -torrent payload.torrent -port 42001

/usr/bin/time -lp ./go-pure-portable -mode fetch -transport tcp \
  -data run-01 -torrent payload.torrent -peer 127.0.0.1:42001
```

Discard only the first successful sample as warm-up. Keep failures and verify
every successful destination against the recorded fixture digest. Do not reuse
a destination: completion state makes that a zero-byte no-op.

The 2026-07-31 run used random payloads created before deterministic generation
was added. Their digests and all retained process samples are preserved under
`bench/results/2026-07-31-engine-bakeoff/`; future runs use the seeded fixture
command above.
