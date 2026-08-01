# Academic Torrents catalogue parser benchmark

The component benchmark parses a generated 2,850-record catalogue through the
same strict streaming XML boundary used by catalogue sync.

## Environment

- MacBook Pro `Mac15,7`
- Apple M3 Pro, 12 cores (6 performance, 6 efficiency), 36 GB memory
- macOS 26.5.2 (25F84)
- Go 1.26.5, `darwin/arm64`, default `GOMAXPROCS=12`

## Command

```sh
go test ./internal/provider/academictorrents -run=^$ \
  -bench=BenchmarkParseCatalog -benchmem -benchtime=1s -count=10
```

## Result

Median parse latency was 13.516 ms. The ten runs reported 74.61–78.72 MB/s,
6,851,466–6,851,563 B/op, and exactly 176,805 allocations/op. This is a
component baseline, not a before/after optimisation claim. Network fetch time
and disk-cache replacement are deliberately excluded.

Ten-second fuzz smoke runs completed without a crash: 815,408 catalogue-parser
executions, 541,295 `Retry-After` executions, and 545,205 cache-filename
executions. Counts are throughput observations, not coverage claims.
