# Blackbeard

[![Checks](https://github.com/keys-i/blackbeard/actions/workflows/checks.yml/badge.svg)](https://github.com/keys-i/blackbeard/actions/workflows/checks.yml)
[![Security](https://github.com/keys-i/blackbeard/actions/workflows/security.yml/badge.svg)](https://github.com/keys-i/blackbeard/actions/workflows/security.yml)
[![QA](https://github.com/keys-i/blackbeard/actions/workflows/qa.yml/badge.svg)](https://github.com/keys-i/blackbeard/actions/workflows/qa.yml)
[![MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Blackbeard is a scriptable BitTorrent CLI for deterministic natural-language
search across open catalogues and bounded inspection of direct magnet or
`.torrent` sources.

> [!IMPORTANT]
> Blackbeard does not bundle piracy indexes, bypass access controls, or make
> downloaded content safe or lawful. You are responsible for the content you
> request and share. Peers and trackers can observe your IP address.

## Status

Blackbeard is under active development. Deterministic query parsing, Academic
Torrents catalogue sync, offline schema-versioned search, and direct-source
metadata inspection are implemented. Live multi-provider search, torrent
transfer, streaming, creation, seeding, and the TUI remain unavailable until
their fixture, security, and benchmark gates pass.

## Build

Go 1.26.5 or newer is required while the first release is being prepared:

```sh
git clone https://github.com/keys-i/blackbeard.git
cd blackbeard/src
go build -o build/blackbeard ./cmd/blackbeard
```

## Current commands

```sh
./build/blackbeard help
./build/blackbeard version
./build/blackbeard search --explain \
  "arm64 Linux image under 2 GiB newest first"
./build/blackbeard providers sync
./build/blackbeard search --offline \
  "climate observations from academic"
./build/blackbeard --output json search --explain \
  '"public domain" animation under 4 GiB from archive'
./build/blackbeard --output json inspect \
  'magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567'
./build/blackbeard inspect ~/Downloads/debian.torrent
# Illustrative: replace this with a publisher's direct HTTPS .torrent URL.
./build/blackbeard inspect https://example.org/catalogue/debian.torrent
```

Versioned installs use release archives. Blackbeard does not publish a Go
library or promise versioned `go install ...@vX` support from the `/src` module.

## Scope

Blackbeard searches only explicitly supported public catalogs and publisher
feeds. The BitTorrent DHT finds peers for a known infohash; it is not a
keyword-search database. HTML scrapers for changing third-party indexes are
deliberately out of scope.

Research, architecture, benchmark methodology, and operator guidance are
published in the [project wiki](https://github.com/keys-i/blackbeard/wiki) as
their evidence lands. Bugs and planned work are tracked in
[GitHub Issues](https://github.com/keys-i/blackbeard/issues).

Security reports follow [SECURITY.md](docs/SECURITY.md).
Contributions follow [CONTRIBUTING.md](docs/CONTRIBUTING.md).

Planned release targets are macOS arm64/amd64, Linux arm64/amd64, and Windows
amd64. Windows arm64 is built in CI as an additional portability check.

## License

[MIT](LICENSE)
