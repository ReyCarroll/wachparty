# Watch Party

Watch a video together with friends, in sync, over Hamachi, Tailscale, ZeroTier or the same Wi-Fi.
The host runs one Windows `.exe`, picks videos, and everyone watches in their browser (PC or phone).

## Features
- Synced play / pause / seek for any number of viewers, with ping compensation; the room waits while anyone buffers.
- Per-viewer buffer control: stream with N seconds buffered, or download the whole file first.
- Shared playlist: add files or whole folders (subfolders kept); anyone can pick what plays; next episode auto-plays.
- Host build checks compatibility and converts what browsers can't play (live while playing, or to a file).
- Subtitles: sidecar `.srt/.vtt/.ass`, embedded tracks, or a file picked by hand.
- QR codes for every network address (Hamachi, Tailscale, ZeroTier, LAN) so phones can join without an app.

## Two builds
| | `WatchParty.exe` (host) | `WatchParty-Lite.exe` (friends) |
|---|---|---|
| Size | ~73 MB (embeds ffmpeg) | ~7 MB |
| Join a room | yes | yes |
| Host a room | yes, with conversion + subtitles | yes, files shared as-is |

Friends don't need a new build for most changes: the player page is served by the host.

## Build
Needs Go 1.21+.

```bash
./build.sh                 # fetches ffmpeg on first run, then builds both .exe files
go build -tags lite .      # lite build only, no ffmpeg needed
go test ./...
go vet ./... && go vet -tags lite ./...
```

`assets/ffmpeg.exe.gz` (not in git, ~66 MB) is created by `scripts/fetch-ffmpeg.sh`.

## Layout
- `main.go` – room state, sync server (SSE + `/api/cmd`), addresses, file dialogs
- `playlist.go` – playlist, folder scan, auto-advance
- `convert.go`, `live.go` – ffmpeg probe, convert-to-file, seekable live HLS conversion (full build)
- `subs.go` – subtitles (full build)
- `lite.go` – stubs for the lite build
- `web/` – host page, player, hls.js, qrcode-generator

## Known limits
Windows-only parts (file dialogs, firewall button, ffmpeg unpacking) are untested on real Windows hosts;
tests and browser checks ran on Linux. Phones can't join Hamachi (use Tailscale/ZeroTier or the same Wi-Fi).
