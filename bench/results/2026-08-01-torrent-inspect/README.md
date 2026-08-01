# Direct-source inspection benchmark

This evidence covers issue #10's bounded magnet and metainfo inspection slice.
It makes no transfer-engine or public-network performance claim.

## Provenance

- Machine: Apple M3 Pro, 12 logical CPUs, 36 GiB RAM, macOS 26.5.2
  (25F84), AC power, low-power mode disabled.
- Go: 1.26.5, darwin/arm64, default `GOMAXPROCS=12`, cgo disabled.
- Filesystem cache: warm and uncontrolled; no cache flush was attempted.
- Security-correct baseline: `4a9fd523cbef05c0b02c1621af3dc47b297f8267`.
- Measured implementation: `2930c9dcf0be3a125216de9f38605356b5fc1f06`.
- Pre-feature binary baseline: `626c507e449ad57db8f55b4b98a5473beb0c1b9f`.

Fixtures are generated in `src/internal/torrent/inspect_test.go`; no external
payload or network is involved. V1, v2, and hybrid each describe one 1-byte
file with a 16 KiB piece length. The metadata-heavy v1 fixture describes 512
one-byte files under `files/` with one piece hash. The same fixture constructors
and benchmark command were used before and after the optimisation.

## Repeated warm parse

Ten 500 ms samples were compared with benchstat. Values are medians; `±` is
benchstat's reported spread.

| Fixture | Time | Bytes/op | Allocs/op |
|---|---:|---:|---:|
| v1 | 3.387 µs ±2% | 3.963 KiB | 62 |
| v2 | 7.514 µs ±5% | 11.84 KiB | 133 |
| hybrid | 10.18 µs ±5% | 13.21 KiB | 176 |
| metadata-heavy | 904.7 µs ±10% | 563.1 KiB | 13,907 |

The retained ASCII path-key fast path did not weaken Unicode case-folding.
Against the security-correct baseline it reduced the geometric-mean time by
4.12%, bytes/op by 11.62%, and allocations/op by 2.61%. Metadata-heavy time was
statistically unchanged (`p=0.853`), while its bytes/op fell 31.27% and its
allocations/op fell 6.86%. V2 time improved 10.35%; other timing differences
were not statistically significant. The full comparison is in `benchstat.txt`.

## First parse in a fresh process

Each row is one descriptive `-benchtime=1x` sample from a newly launched test
binary. The benchmark timer covers its first parse; RSS includes the Go test
runtime and fixture construction.

| Fixture | First parse | Bytes/op | Allocs/op | Process max RSS |
|---|---:|---:|---:|---:|
| v1 | 46.625 µs | 11,680 | 132 | 13,713,408 B |
| v2 | 43.333 µs | 18,320 | 203 | 13,910,016 B |
| hybrid | 57.291 µs | 23,256 | 272 | 13,975,552 B |
| metadata-heavy | 955.917 µs | 585,400 | 13,999 | 14,860,288 B |

## Binary size

The Darwin arm64 cgo-free binary grew from 24,834,546 to 26,201,298 bytes:
1,366,752 bytes, or 5.5034%. Both builds used:

```sh
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath \
  -ldflags='-X github.com/keys-i/blackbeard/src/internal/version.Version=benchmark' \
  -o /private/tmp/blackbeard-inspect-<before|after> ./cmd/blackbeard
```

## Profiles

A 3-second metadata-heavy run completed with CPU, heap, and runtime-trace
outputs (`892,329 ns/op`, `576,750 B/op`, `13,907 allocs/op`). Binary profile
files are not committed because the repository structure gate rejects generated
profiles; the benchmark workflow now uploads the same three files as retained
CI artefacts.

## Commands and limits

```sh
go test -run '^$' \
  -bench '^BenchmarkInspect(V1|V2|Hybrid|MetadataHeavy)$' \
  -benchmem -benchtime=500ms -count=10 ./internal/torrent
benchstat baseline.txt raw.txt
go test -c -o /private/tmp/blackbeard-torrent.test ./internal/torrent
/usr/bin/time -l /private/tmp/blackbeard-torrent.test \
  -test.run '^$' -test.bench '^BenchmarkInspectV1$' \
  -test.benchtime=1x -test.count=1
```

The cold rows and CLI RSS samples are single observations, not regression
thresholds. Network, disk payload I/O, engine throughput, and cross-machine
comparisons are outside this slice.
