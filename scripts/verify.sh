#!/usr/bin/env bash
# 参照実装 (solutions/) がコンパイルできることを確認する。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

echo "[mini-ad] go mod tidy"
go mod tidy

pkgs=()
for d in ./solutions/*/; do
  [ -f "${d}main.go" ] || continue
  pkgs+=("$d")
done
for d in ./steps/step07-rtb/mini-ad ./steps/step07-rtb/mock-dsp; do
  [ -f "${d}/main.go" ] && pkgs+=("$d")
done
if [ ${#pkgs[@]} -eq 0 ]; then
  echo "[mini-ad] no packages to verify"
  exit 0
fi

echo "[mini-ad] go build ${pkgs[*]}"
go build -o /dev/null "${pkgs[@]}"

echo "[mini-ad] OK"
