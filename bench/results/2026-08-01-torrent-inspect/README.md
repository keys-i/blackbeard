# Direct-source inspection benchmark

This evidence covers issue #10's bounded magnet and metainfo inspection slice.
It makes no transfer-engine or public-network performance claim.

## Provenance

- Machine: Apple M3 Pro, 12 logical CPUs, 36 GiB RAM, macOS 26.5.2
  (25F84), AC power, low-power mode disabled.
- Go: 1.26.5, darwin/arm64, default `GOMAXPROCS=12`, cgo disabled.
- Filesystem cache: warm and uncontrolled; no cache flush was attempted.
- Measured implementation: `45f0caa0bc7c4cb09599d865cbcd6f51853fe626`.
- Security-correct baseline: the same revision with only
  [`baseline.patch`](baseline.patch) applied to remove the ASCII path-key fast
  path.
- Pre-feature binary baseline: `626c507e449ad57db8f55b4b98a5473beb0c1b9f`.

Fixtures are generated in `src/internal/torrent/inspect_test.go`; no external
payload or network is involved. V1, v2, and hybrid each describe one 1-byte
file with a 16 KiB piece length. V2 and hybrid include the mandatory empty
BEP 52 `piece layers` dictionary. The metadata-heavy v1 fixture describes 512
one-byte files under `files/` with one piece hash. The same fixture bytes and
benchmark command were used before and after the optimisation.

## Repeated warm parse

Ten 500 ms samples were compared with benchstat. Values are medians; `±` is
benchstat's reported spread.

| Fixture | Time | Bytes/op | Allocs/op |
|---|---:|---:|---:|
| v1 | 3.283 µs ±1% | 3.963 KiB | 62 |
| v2 | 7.485 µs ±1% | 11.93 KiB | 138 |
| hybrid | 10.32 µs ±3% | 13.30 KiB | 181 |
| metadata-heavy | 822.8 µs ±1% | 563.0 KiB | 13,907 |

The retained ASCII path-key fast path did not weaken Unicode case-folding.
Against the security-correct baseline it reduced the geometric-mean time by
4.55%, bytes/op by 11.61%, and allocations/op by 2.60%. Metadata-heavy time
fell 7.96%, bytes/op fell 31.27%, and allocations/op fell 6.86%. V2 time fell
2.41%. All measured timing differences were significant at `p<0.001` with ten
samples. The full comparison is in `benchstat.txt`.

## First parse in a fresh process

Each row is one descriptive `-benchtime=1x` sample from a newly launched test
binary. The benchmark timer covers its first parse; RSS includes the Go test
runtime and fixture construction.

| Fixture | First parse | Bytes/op | Allocs/op | Process max RSS |
|---|---:|---:|---:|---:|
| v1 | 218.292 µs | 11,728 | 133 | 13,942,784 B |
| v2 | 72.042 µs | 19,120 | 218 | 14,106,624 B |
| hybrid | 62.958 µs | 23,512 | 282 | 13,910,016 B |
| metadata-heavy | 859.709 µs | 585,400 | 13,999 | 14,974,976 B |

## Binary size

The Darwin arm64 cgo-free binary grew from 24,834,546 to 26,201,298 bytes:
1,366,752 bytes, or 5.5034%. Both builds used:

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath \
  -ldflags='-X github.com/keys-i/blackbeard/src/internal/version.Version=benchmark' \
  -o /private/tmp/blackbeard-inspect-<before|after> ./cmd/blackbeard
```

`go version -m` records `vcs.modified=false` for both binaries, at revisions
`626c507e449ad57db8f55b4b98a5473beb0c1b9f` and
`77fa0154a426591b951bc6dfd6603fd7d7152377` respectively; the captured build
metadata is in [`binary-buildinfo.txt`](binary-buildinfo.txt).

## Profiles

A 3-second metadata-heavy run completed with CPU, heap, and runtime-trace
outputs (`821,790 ns/op`, `576,724 B/op`, `13,907 allocs/op`). Binary profile
files are not committed because the repository structure gate rejects generated
profiles; the benchmark workflow now uploads the same three files as retained
CI artefacts.

## Commands and limits

```sh
BLACKBEARD_REPO=$(pwd)
BLACKBEARD_BENCH=$(mktemp -d)
git worktree add --detach "$BLACKBEARD_BENCH/tree" \
  45f0caa0bc7c4cb09599d865cbcd6f51853fe626
git -C "$BLACKBEARD_BENCH/tree" apply --check \
  "$BLACKBEARD_REPO/bench/results/2026-08-01-torrent-inspect/baseline.patch"
git -C "$BLACKBEARD_BENCH/tree" apply \
  "$BLACKBEARD_REPO/bench/results/2026-08-01-torrent-inspect/baseline.patch"
cd "$BLACKBEARD_BENCH/tree/src"
CGO_ENABLED=0 go test -run '^$' \
  -bench '^BenchmarkInspect(V1|V2|Hybrid|MetadataHeavy)$' \
  -benchmem -benchtime=500ms -count=10 ./internal/torrent \
  > "$BLACKBEARD_BENCH/baseline.txt"
cd ..
git apply --reverse \
  "$BLACKBEARD_REPO/bench/results/2026-08-01-torrent-inspect/baseline.patch"
git diff --quiet -- src/internal/torrent/inspect.go
cd src
CGO_ENABLED=0 go test -run '^$' \
  -bench '^BenchmarkInspect(V1|V2|Hybrid|MetadataHeavy)$' \
  -benchmem -benchtime=500ms -count=10 ./internal/torrent \
  > "$BLACKBEARD_BENCH/current.txt"
go run golang.org/x/perf/cmd/benchstat@v0.0.0-20260709024250-82a0b07e230d \
  "$BLACKBEARD_BENCH/baseline.txt" "$BLACKBEARD_BENCH/current.txt"
cd "$BLACKBEARD_REPO"
git worktree remove "$BLACKBEARD_BENCH/tree"
cd src
go test -c -o /private/tmp/blackbeard-torrent.test ./internal/torrent
/usr/bin/time -l /private/tmp/blackbeard-torrent.test \
  -test.run '^$' -test.bench '^BenchmarkInspectV1$' \
  -test.benchtime=1x -test.count=1
```

The cold rows and CLI RSS samples are single observations, not regression
thresholds. Network, disk payload I/O, engine throughput, and cross-machine
comparisons are outside this slice.
