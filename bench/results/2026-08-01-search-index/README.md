# Offline index benchmark

This run measures the provenance-correct Bleve index before and after removing
merged-record construction from the search hot path, avoiding repeated stored
source encoding, and narrowing indexed fields. The bounded result collector and
hostile-input limits are included in the after build.

## Environment

- MacBook Pro `Mac15,7`
- Apple M3 Pro, 12 cores (6 performance, 6 efficiency), 36 GB memory
- macOS 26.5.2 (25F84)
- Go 1.26.5, `darwin/arm64`, default `GOMAXPROCS=12`
- filesystem cache not cleared; `SearchColdOpen` means a fresh Bleve handle,
  not a cold OS page cache

## Command

```sh
go test ./internal/index -run=^$ \
  -bench='Benchmark(IndexRebuild|SearchWarm|SearchColdOpen)$' \
  -benchmem -benchtime=100ms -count=10
```

Compared with `benchstat` from `golang.org/x/perf` commit
`82a0b07e230d` (2026-07-09).

## Result

| Component | Before | After | Decision |
|---|---:|---:|---|
| Rebuild latency | 141.1 ms | 145.6 ms | no significant change (`p=0.190`) |
| Warm search latency | 1.530 ms | 0.792 ms | retain, 48.26% lower (`p<0.001`) |
| Open-and-search latency | 9.901 ms | 10.342 ms | no significant change (`p=0.105`) |
| Rebuild allocation | 34.57 MiB | 28.24 MiB | retain, 18.33% lower |
| Warm search allocation | 1010.1 KiB | 572.4 KiB | retain, 43.33% lower |
| Open-and-search allocation | 3.277 MiB | 2.516 MiB | retain, 23.23% lower |

The before build was an uncommitted, corrected implementation snapshot captured
in the tool transcript. Its raw output is preserved here, but it cannot be
checked out by Git ref; this comparison is evidence for the retained local
changes, not a release regression gate. Future comparisons must use committed
revisions on the same machine and benchmark settings.
