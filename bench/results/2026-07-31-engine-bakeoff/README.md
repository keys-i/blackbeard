# Engine bake-off — 2026-07-31

These are raw process samples from a controlled loopback spike, not public
network or final-product performance claims.

## Environment

- MacBook Pro `Mac15,7`, Apple M3 Pro, 12 cores (6 performance, 6 efficiency)
- 36 GB RAM
- macOS 26.5.2 build 25F84, Darwin 25.5.0, arm64
- Go 1.26.5
- Rust 1.97.1 (`8bab26f4f`, LLVM 22.1.6)
- Cargo 1.97.1 (`c980f4866`)
- APFS/NVMe, warm filesystem cache
- `RLIMIT_NOFILE` soft limit 256
- runtime-default CPU scheduling; no `GOMAXPROCS` override
- host reported AC power and a discharging battery simultaneously
- low-power mode disabled

The contradictory power state makes these unsuitable for power-efficiency
claims. Thermal telemetry, OS-thread samples, open-descriptor samples, disk
write amplification, GC traces, and pprof profiles were not captured in this
first spike and are not inferred.

## Fixtures

| Fixture | Bytes | Piece bytes | Payload SHA-256 | Metainfo SHA-256 |
|---|---:|---:|---|---|
| TCP | 536,870,912 | 1,048,576 | `7238f090173aa1efa7e960ff995dca3b379f881ef1bfc438dd2b4552703c89c5` | `21a33a82f1286cb392b00c1db12967d2af2b2f5d44205f1dae1a3c9b0004f884` |
| uTP | 67,108,864 | 262,144 | `457a19475fae4400dc1b6d8f6ba88180e6f01502f3976f7caf7eb2f3e4b143a3` | `a116eee94dd0427ce6ef1974b984feaf4640042bfee87dd13e599385337ef873` |

The measured fixtures were random and are identified by digest; they are not
committed. The retained harness now generates bit-for-bit deterministic future
fixtures from a named seed.

Discovery was disabled and each attempt used one fresh fixed loopback seeder,
one fresh destination, and the same metadata. Successful destinations were
compared with the source digest. `/usr/bin/time -lp` supplied the raw process
columns. The first success in each cohort is labelled `warmup`; it is preserved
but excluded from summaries. Failures remain labelled rather than disappearing
from the dataset.

## Engine identities and builds

| Arm | Version/build | Binary bytes | Binary SHA-256 |
|---|---|---:|---|
| Go native | anacrolix/torrent v1.61.0, `CGO_ENABLED=1` | 20,012,162 | `328479aea75fd1cc72239382d65ea9df22a1c62e4e4d025e987f700984700dfb` |
| Go pure cgo control | v1.61.0, `CGO_ENABLED=1 -tags=disable_libutp` | 20,032,530 | `537c18a4508574cbd03ebbb5f80b8425a6be459822b7b31679ecbd531494585b` |
| Go pure portable | v1.61.0, `CGO_ENABLED=0` | 19,006,994 | `f18df1e584dd1d127decf1eef6a3b329c9ce545cda0d03c9001fc4fca0c96cdd` |
| Rust | rqbit v8.1.1 official locked build | 17,317,696 | `6e1504681023dba9a4f763b32fb0cf66a8c75c58699ca55d74c7325ff4dda6f4` |

The binaries are benchmark harnesses/CLI tools, not Blackbeard release-size
measurements.

## Retained summaries

The observed p95 is the nearest-rank sample percentile, not a confidence
interval. CPU is user plus system time. MiB uses 1,048,576 bytes.

| Engine arm | Payload | n | Real p50 | Real p95 | CPU p50 | RSS p50 | RSS p95 |
|---|---:|---:|---:|---:|---:|---:|---:|
| Go pure portable TCP | 512 MiB | 9 | 0.78 s | 0.88 s | 1.39 s | 434.8 MiB | 449.6 MiB |
| rqbit stable TCP | 512 MiB | 5 | 0.75 s | 15.23 s | 0.91 s | 17.2 MiB | 17.5 MiB |
| Go native uTP | 64 MiB | 9 | 0.60 s | 0.63 s | 0.87 s | 91.9 MiB | 92.3 MiB |
| Go pure uTP, cgo control | 64 MiB | 9 | 0.55 s | 1.94 s | 1.01 s | 93.3 MiB | 94.0 MiB |
| Go pure portable uTP | 64 MiB | 9 | 0.55 s | 1.85 s | 1.05 s | 92.5 MiB | 93.6 MiB |

Rust had one non-connecting measured attempt after warm-up. Two successful
attempts took 14.55 s and 15.23 s. The failure left a full-size but invalid
preallocated file. The TCP rows are product-level observations, not a language
microbenchmark: the process and storage implementations differ.

## Storage follow-up

The storage cohorts used the 512 MiB TCP fixture, cgo-free Go build, fresh
destinations, and five samples after warm-up.

| Storage arm | Cohort | Real p50 | CPU p50 | RSS p50 |
|---|---|---:|---:|---:|
| Ordinary file I/O | in-memory completion | 1.11 s | 1.54 s | 27.0 MiB |
| mmap | in-memory completion | 0.73 s | 1.06 s | 508.9 MiB |
| mmap | stock persistent completion | 0.89 s | 1.44 s | 464.5 MiB |
| Ordinary file I/O | stock persistent completion | 3.60 s | 2.09 s | 28.6 MiB |

Only the two in-memory-completion rows form the intended classic-versus-mmap
pair. The persistent backend flushes each completed piece: `msync` for mmap and
`File.Sync` for ordinary I/O. The ordinary-I/O environment switch also affected
the seeder, which logged failed part-file flushes before the timed fetch. Source
cache, thermal state, and arm order were not strictly controlled. The speed
ratio is therefore exploratory, not a release-grade destination-storage claim.

The memory result and rooted-storage security requirement justify starting
Blackbeard with ordinary file I/O and grouped durability checkpoints. No mmap
toggle is exposed without a stricter alternating-arm rerun.

## Rejected experiments

1. Reused destinations produced one transfer and nine zero-byte no-ops.
2. A rate-limited long-lived seeder stalled later attempts and left unverified
   full-size part files.
3. A long-lived native seeder stalled on its eighth direct-peer connection.
4. A 512 MiB pure-Go uTP smoke took 64.62 s versus 4.20 s native; the repeated
   64 MiB run did not reproduce that median gap.
5. Equal rqbit minimum and maximum TCP ports created an empty range.
6. A reduced-feature rqbit build failed because removed features also removed
   Serde support needed for `Arc<str>`.
7. One rqbit sample did not connect and ended after 93.70 s.

These experiments were rejected for the stated methodological reasons, not
because their results were inconvenient.
