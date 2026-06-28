#!/usr/bin/env bash
FLAKE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
exec nix develop "$FLAKE_DIR" --command go run github.com/go-delve/delve/cmd/dlv@latest "$@"
