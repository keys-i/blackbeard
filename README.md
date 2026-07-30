# Blackbeard

[![Checks](https://github.com/keys-i/blackbeard/actions/workflows/checks.yml/badge.svg)](https://github.com/keys-i/blackbeard/actions/workflows/checks.yml)
[![Quality](https://github.com/keys-i/blackbeard/actions/workflows/quality.yml/badge.svg)](https://github.com/keys-i/blackbeard/actions/workflows/quality.yml)
[![MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Blackbeard is a fast, terminal-native BitTorrent finder and downloader for
open, public-domain, and otherwise authorized material. It turns plain-language
queries into searches across approved catalogs, then reconstructs verified
files from a torrent without leaving the terminal.

> [!IMPORTANT]
> Blackbeard does not bundle piracy indexes, bypass access controls, or make
> downloaded content safe or lawful. You are responsible for the content you
> request and share. Peers and trackers can observe your IP address.

## Why Blackbeard?

- natural queries such as `Fedora 44 KDE for arm64` or
  `open astronomy dataset under 20 GB`
- trusted-source results from official publishers and open-data catalogs
- interactive terminal selection plus deterministic plain and JSON output
- magnet links and HTTPS `.torrent` files
- measured component and end-to-end benchmarks
- one native executable; no browser, daemon, account, or AI API required

## Install

Go 1.26 or newer is required while the first release is being prepared:

```sh
git clone https://github.com/keys-i/blackbeard.git
cd blackbeard/cli
go install ./cmd/blackbeard
```

## Use

```sh
# Interactive search
blackbeard

# Scriptable search
blackbeard search "Fedora 44 KDE arm64"
blackbeard search --json "open machine-learning dataset under 5 GB"

# Inspect before joining a swarm
blackbeard inspect "magnet:?xt=urn:btih:..."

# Download authorized material
blackbeard fetch --output ./downloads "magnet:?xt=urn:btih:..."
```

Run `blackbeard help` for the compact command reference. The example
configuration is [.config/blackbeard.toml](.config/blackbeard.toml).

## Scope

Blackbeard searches only explicitly supported public catalogs and publisher
feeds. The BitTorrent DHT finds peers for a known infohash; it is not a
keyword-search database. HTML scrapers for changing third-party indexes are
deliberately out of scope.

The embedded torrent engine is
[`anacrolix/torrent`](https://github.com/anacrolix/torrent), licensed under
MPL-2.0. Its corresponding source is available from that project and through
Go module tooling. Blackbeard's own source is MIT licensed.

Research, architecture, benchmark methodology, and operator guidance live in
the [project wiki](https://github.com/keys-i/blackbeard/wiki). Bugs and planned
work are tracked in [GitHub Issues](https://github.com/keys-i/blackbeard/issues).

## License

[MIT](LICENSE)
