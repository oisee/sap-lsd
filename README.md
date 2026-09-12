# sap-lsd — SAP Light-Show Dispatcher

A tiny, self-contained **rogue SAP GUI server**: it speaks the SAP **DIAG**
protocol well enough that a real SAP GUI (or the bundled `sap-tui` terminal
viewer) draws a demoscene light-show from its frames — spinning cubes, a
tornado, LED plasma, and a greetings finale, all made of real GUI widgets
(labels, buttons, input fields, the classic-list colour channel).

*"Dispatcher"* is the SAP term for the process that listens on the DIAG port
(`32NN`) — which is exactly what this is. **LSD** = light-show, in the old
demoscene sense.

Two commands:

| | what | run |
|---|---|---|
| **sap-lsd** | the server — plays the show to any SAP GUI | `./sap-lsd -listen :3200` |
| **sap-tui** | a terminal viewer, for watching without a SAP GUI | `sap-tui HOST:3200` |

## Watch it live

A public instance runs at **`demo.desude.su:3200`**.

- **SAP GUI**: Application Server `demo.desude.su`, Instance Number **00**, no SNC.
- **No SAP GUI?** Use **sap-tui** — the terminal viewer, its own repo and prebuilt
  binaries (Linux / macOS / Windows): **https://github.com/oisee/sap-tui**

  ```sh
  # download a binary from the release, or:
  go install github.com/oisee/sap-tui/cmd/sap-tui@latest
  sap-tui demo.desude.su:3200
  ```

## Build

```sh
make build        # -> bin/sap-lsd, bin/sap-tui
make run          # build + run the server on :3200
make viewers      # cross-compiled sap-tui for linux / macOS
make dist         # a deployable bundle: dist/sap-lsd.tgz
```

Pure Go, no cgo. `sap-lsd` with no arguments plays the composed show on `:3200`
from the scrubbed asset in `assets/`.

## Connect

- **SAP GUI**: Application Server = `HOST`, Instance Number = **00**, no SNC.
  (SAP instance `NN` maps to dispatcher port `32NN`; `:3200` = instance 00.
  Change `-listen :32NN` to move it, e.g. `:3212` = instance 12.)
- **sap-tui**: `sap-tui HOST:3200` — read-only, `q` quits. It lives in its own
  repo with prebuilt binaries for Linux / macOS / Windows:
  **https://github.com/oisee/sap-tui** (`make viewers` here builds copies into `dist/`).

## Deploy (systemd / cloud VM)

```sh
make dist
scp dist/sap-lsd.tgz user@HOST:~
ssh user@HOST 'tar xzf sap-lsd.tgz && cd sap-lsd && ./install-on-vm.sh'
```

Installs to `/opt/sap-lsd`, runs as a systemd service on TCP `:3200`. Open that
one port in your firewall / cloud NSG and point a DNS A-record at the host.
DIAG is unencrypted — but there is nothing to protect here, it is a one-way
light-show; keep only the show port (and SSH) open.

## The show file

`assets/show.json` is the timeline (scene, seconds, per-scene speed). Edit it and
restart — or run `sap-lsd -http :8088` for the in-browser composer (keep that
port private; it has no auth).

## Privacy: the scrubbed asset

`assets/probe.scrubbed.jsonl` is the DIAG backdrop the scenes are spliced into,
**scrubbed of every real identifier** (session GUIDs, hostnames, IPs, user
names) — same length in, same length out, so the frames stay valid. If you
record a fresh backdrop, re-scrub it: edit the token table in
`tools/scrub/scrub.py` and `make scrub IN=your-capture.jsonl`. Never commit an
unscrubbed capture (`.gitignore` blocks the usual names).

## Provenance

`internal/` vendors the packages this needs, unchanged, from the author's other
projects: the DIAG codec, canvas, scene engine and ALV/LZH from
`open-diag-go-pro`, the NI network layer from `open-rfc-go`, and `sapcompress`
from `vibing-steampunk`. This repo is the shippable light-show carved out of
them.

MIT licensed. Not affiliated with or endorsed by SAP SE. "SAP" is a trademark
of SAP SE; used here only to name the protocol this speaks.
