#!/usr/bin/env bash
# Downloads a Windows ffmpeg (GPL build, includes libx264) and stores it gzip-compressed at assets/ffmpeg.exe.gz,
# which the full build embeds via go:embed. Not needed for the lite build (-tags lite).
set -euo pipefail
cd "$(dirname "$0")/.."
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fL -o "$tmp/ff.zip" https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip
unzip -o -j "$tmp/ff.zip" '*/bin/ffmpeg.exe' -d "$tmp" >/dev/null
mkdir -p assets
gzip -9 -c "$tmp/ffmpeg.exe" > assets/ffmpeg.exe.gz
ls -la assets/ffmpeg.exe.gz
