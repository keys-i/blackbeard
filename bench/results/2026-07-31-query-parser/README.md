# Query parser — 2026-07-31

Environment: Apple M3 Pro, macOS 26.5.2 build 25F84, Go 1.26.5,
Darwin/arm64, runtime-default `GOMAXPROCS=12`. Filesystem and network are not
involved.

Command:

```sh
(
  cd src
  go test -run '^$' \
    -bench 'Benchmark(ParseStructured|ParseLexical|Tokenize|ParseByteQuantity)$' \
    -benchmem -count=10 ./internal/query
)
```

The normalization change under comparison is preserved in
[`normalization-fast-path.patch`](normalization-fast-path.patch). From the
retained source revision, reproduce both sides from the repository root:

```sh
git apply --unidiff-zero --reverse \
  bench/results/2026-07-31-query-parser/normalization-fast-path.patch
# Run the benchmark command above for the naive baseline.
git apply --unidiff-zero \
  bench/results/2026-07-31-query-parser/normalization-fast-path.patch
# Run the same benchmark command for the retained implementation.
```

The two raw outputs and their `benchstat` comparison are retained beside this
file. Compiler and host metadata are recorded here and in the raw headers; the
retained source revision is the Git commit that introduces this directory.

Adding NFKC plus Unicode case folding naively to every normalization call
preserved correctness but caused a severe ASCII-query regression. The retained
version first proves the input is ASCII, where lowercase is exactly equivalent,
and invokes the Unicode normalization path only otherwise.

| Benchmark | Naive NFKC/case fold median | ASCII fast-path median | Change |
|---|---:|---:|---:|
| Structured parse | 5.35 µs | 3.01 µs | -43.8%, p=0.000 |
| Lexical parse | 17.81 µs | 4.01 µs | -77.5%, p=0.000 |
| Structured bytes/op | 10,502 | 3,329 | -68.3% |
| Lexical bytes/op | 44,912 | 1,648 | -96.3% |
| Structured allocations | 76 | 48 | -36.8% |
| Lexical allocations | 184 | 15 | -91.8% |

Tokenization and exact unit parsing are unchanged by the patch. The later
fast-path run shows host noise in its final tokenization samples; the parser
improvements are much larger than that variance, but the raw rows are retained
for benchstat rather than edited.

## Portable binary size

The same Go 1.26.5 toolchain and `CGO_ENABLED=0 go build -trimpath` flags were
used for scaffold commit `cd6dd1a` and this slice:

| Target | Scaffold | Query/CLI slice | Delta |
|---|---:|---:|---:|
| Darwin arm64 | 2,512,450 B | 4,836,898 B | +2,324,448 B |
| Darwin amd64 | 2,601,280 B | 5,039,008 B | +2,437,728 B |
| Linux arm64 | 2,504,912 B | 4,697,716 B | +2,192,804 B |
| Linux amd64 | 2,539,656 B | 4,871,038 B | +2,331,382 B |
| Windows amd64 | 2,617,856 B | 5,065,728 B | +2,447,872 B |

This delta includes the Cobra command surface, JSON/NDJSON/table contracts,
Unicode normalization tables, query parser, and terminal sanitizer. Replacing
Cobra's general version-template path with Blackbeard's already-required output
encoder removed 1.83–2.01 MiB from the five binaries while making help and
version obey machine-output mode; the before/after bytes are retained in
[`machine-output-size.csv`](machine-output-size.csv). It is retained as the
baseline for later dependency and release-flag decisions; no stripped-size
number is substituted for this debuggable build.
