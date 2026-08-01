# Security Policy

## Supported version

Only the current default branch and latest published release are supported.

## Reporting a vulnerability

Do not open a public issue for a vulnerability that exposes secrets, private
data, unsafe path handling, SSRF, code execution, or a practical exploit.

Private vulnerability reporting is not currently enabled. Contact the
maintainer, `@keys-i`, through an existing private channel before sending
sensitive details. This section will point to GitHub Security Advisories only
after that repository setting is verified.

Include:

- a short description of the problem and its impact
- the affected command, file, workflow, or dependency
- the minimum safe steps needed to reproduce it
- relevant logs or a proof of concept with secrets and harmful payloads removed

Ordinary provider outages, stale results, and display problems are not
security vulnerabilities; use the bug template for those.

## User safety

Torrent hashes verify bytes against metainfo; they do not establish publisher
identity, legality, or malware safety. Blackbeard never executes, mounts, or
extracts downloaded payloads. An explicitly configured media player is invoked
with an argument array, never through a shell. Peers and trackers can observe
network addresses, and normal BitTorrent participation uploads pieces to other
peers.
