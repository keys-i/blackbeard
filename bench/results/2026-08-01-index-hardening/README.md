# Catalogue generation hardening benchmark

This comparison measures issue #9's lifecycle change: an absent catalogue no
longer creates and swaps an empty bootstrap Bleve generation, while cold opens
validate the live and sibling generation nodes with `Lstat` before use.

## Environment

- MacBook Pro `Mac15,7`
- Apple M3 Pro, 12 cores (6 performance, 6 efficiency), 36 GB memory
- macOS 26.5.2 (25F84)
- Go 1.26.5, `darwin/arm64`, runtime-default `GOMAXPROCS=12`
- APFS/NVMe; filesystem cache not cleared
- AC power; low-power mode disabled

The before build is `origin/main` at
`2039fd1bf077bf682811a6411d5380cd03c2ed30`; the after build is the commit
containing this artifact. Both runs used the same machine and command:

```sh
go test ./internal/index -run '^$' \
  -bench '^(BenchmarkIndexRebuild|BenchmarkSearchColdOpen)$' \
  -benchmem -benchtime=100ms -count=10
```

Compared with `benchstat` from `golang.org/x/perf` commit
`82a0b07e230d` (2026-07-09).

An intermediate after sample overlapped an independent review run on the same
machine and was discarded. `after.txt` is the subsequent uncontended sample.

## Result

| Component | Before | After | Decision |
|---|---:|---:|---|
| First rebuild latency | 152.5 ms | 114.1 ms | retain, 25.14% lower (`p<0.001`) |
| Cold open and search | 10.79 ms | 10.55 ms | no significant change (`p=0.579`) |
| First rebuild allocation | 27.72 MiB | 27.53 MiB | no significant change (`p=0.315`) |
| Cold open and search allocation | 2.554 MiB | 2.524 MiB | no significant change (`p=0.063`) |

The rebuild win comes from deleting the empty bootstrap generation and its
extra close/rename work. The additional generation-node and legacy-migration
checks did not produce a statistically significant cold-open latency or
allocation regression. No search hot-path claim is made.
