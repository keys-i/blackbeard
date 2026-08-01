# Catalogue CLI baseline

This component baseline exercises the complete offline command path over a
prebuilt 100-record Bleve index: Cobra construction, deterministic query parse,
index open, lexical search, schema-versioned JSON encoding, and index close.
It excludes provider network sync and filesystem-cache cold-start claims.

## Environment

- MacBook Pro `Mac15,7`
- Apple M3 Pro, 12 cores (6 performance, 6 efficiency), 36 GB memory
- macOS 26.5.2 (25F84)
- Go 1.26.5, `darwin/arm64`, runtime-default `GOMAXPROCS=12`
- APFS/NVMe; filesystem cache not cleared
- AC power; low-power mode disabled
- `RLIMIT_NOFILE=256`

## Command

```sh
go test ./internal/app -run '^$' \
  -bench '^BenchmarkSearchOfflineCLI$' \
  -benchmem -benchtime=100ms -count=10
```

## Result

| Metric | Ten-sample observation |
|---|---:|
| latency median | 10.251 ms/op |
| latency range | 9.626–10.835 ms/op |
| allocation median | 577,061 B/op |
| allocation range | 574,699–584,629 B/op |
| allocations range | 4,734–4,764 allocs/op |

This is an absolute baseline, not an optimisation claim. The dominant work is
opening and closing the persistent index for a one-shot CLI invocation; a
daemon or retained global handle is deliberately out of scope.

## Portable binary size

All binaries used `CGO_ENABLED=0 go build -trimpath`. The before build is
`origin/main` at `45fa540835e0bafd795734a980cef2a187c1269d`; the after build is
this worktree before commit.

| Target | Bytes |
|---|---:|
| Darwin arm64 | 24,817,474 |
| Darwin amd64 | 26,343,440 |
| Linux arm64 | 23,990,973 |
| Linux amd64 | 25,682,516 |
| Windows amd64 | 26,166,272 |

The Darwin arm64 baseline was 4,820,546 bytes. Linking the already-selected
Bleve implementation into the real CLI adds 19,996,928 bytes (414.8%). The
increase is retained as the measured cost of offline indexing; it is not
described as a performance improvement.
