#!/usr/bin/env bash
# Cross-compiles both Windows executables from Linux/macOS.
set -euo pipefail
cd "$(dirname "$0")"
[ -f assets/ffmpeg.exe.gz ] || ./scripts/fetch-ffmpeg.sh
export GOOS=windows GOARCH=amd64
go build -ldflags="-s -w" -o WatchParty.exe .                 # host: ~73 MB (embeds ffmpeg: conversion, live conversion, subtitles)
go build -tags lite -ldflags="-s -w" -o WatchParty-Lite.exe . # friends: ~7 MB (no ffmpeg)
ls -la *.exe
