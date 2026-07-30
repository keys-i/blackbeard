# Contributing

Blackbeard is a lawful-content discovery and BitTorrent download tool. Changes
should improve correctness, safety, performance, accessibility, or the
terminal experience without expanding it into a piracy index.

## Before contributing

- Open an issue before substantial work. A direct pull request is fine for a
  typo, small bug, or obvious cleanup.
- Keep each change focused and include benchmark evidence for performance
  claims.
- Reuse the standard library and existing packages before adding a dependency
  or abstraction.

## Boundaries

- Do not add piracy indexes, private-tracker integrations, access-control
  bypasses, DHT crawling, or DRM circumvention.
- Do not include copyrighted test media. Use generated fixtures or material
  with clear redistribution rights.
- Treat provider data, metainfo, tracker URLs, file paths, and peer traffic as
  untrusted input.
- Check technical claims rather than treating AI output as a source. Disclose
  substantial AI assistance when it affects review or attribution.

By contributing, you confirm that you may share the material and agree that it
will be distributed under the repository's [MIT License](../LICENSE).

## Checks

Requires Go 1.26 or newer:

```sh
cd cli
gofmt -w .
go vet ./...
go test -race ./...
go test -run '^$' -bench . -benchmem ./...
```

For terminal changes, also check a narrow terminal, `NO_COLOR=1`, redirected
output, and keyboard-only operation.

## Pull requests

Explain what changed and why, link the issue, and list the checks used. Include
before-and-after benchmark samples for optimizations. Screenshots are useful
only when the visible terminal layout changed.
