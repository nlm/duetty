#!/usr/bin/env bash
# Builds a self-contained duetty release archive for the current platform:
#
#   dist/duetty-<version>-<os>-<arch>.tar.gz (+ .sha256)
#
# The archive holds the duetty binary with the models and onnxruntime library
# it looks up next to itself (models/, third_party/onnxruntime/lib/), the
# denoiser model, README, LICENSE, config.example.toml and the licenses of
# everything bundled. Run from anywhere; it installs the models first
# (scripts/setup-models.sh, scripts/setup-denoise.sh).
#
# Usage: scripts/package-release.sh VERSION [OUTDIR]
set -euo pipefail

VERSION="${1:?usage: package-release.sh VERSION [OUTDIR]}"
VERSION="${VERSION#v}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${2:-$ROOT/dist}"
GOOS="$(go env GOOS)"
GOARCH="$(go env GOARCH)"
NAME="duetty-$VERSION-$GOOS-$GOARCH"

"$ROOT/scripts/setup-models.sh"
"$ROOT/scripts/setup-denoise.sh"

ORTROOT="$ROOT/third_party/onnxruntime"
if [ ! -f "$ORTROOT/LICENSE" ]; then
  echo "error: $ORTROOT/LICENSE missing: remove third_party/onnxruntime and re-run" \
    "scripts/setup-models.sh so onnxruntime is downloaded with its license" >&2
  exit 1
fi

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
dir="$stage/$NAME"
mkdir -p "$dir/models/smart-turn" "$dir/models/denoise" \
  "$dir/third_party/onnxruntime/lib" "$dir/licenses/go"

echo "Building duetty $VERSION for $GOOS/$GOARCH..."
(cd "$ROOT" && CGO_ENABLED=1 go build -trimpath \
  -ldflags "-s -w -X main.version=$VERSION" -o "$dir/duetty" ./cmd/duetty)

cp "$ROOT/models/silero_vad.onnx" "$dir/models/"
cp "$ROOT/models/smart-turn/smart-turn-v3.2-cpu.onnx" "$dir/models/smart-turn/"
cp "$ROOT/models/denoise/gtcrn_simple.onnx" "$dir/models/denoise/"
# The shared library and its symlinks only (no cmake/pkgconfig files).
cp -P "$ORTROOT"/lib/libonnxruntime* "$dir/third_party/onnxruntime/lib/"
cp "$ROOT/README.md" "$ROOT/LICENSE" "$ROOT/config.example.toml" "$dir/"

# Licenses of everything bundled: models, onnxruntime, Go modules.
cp "$ROOT"/packaging/licenses/*.txt "$dir/licenses/"
cp "$ORTROOT/LICENSE" "$dir/licenses/onnxruntime.txt"
[ -f "$ORTROOT/ThirdPartyNotices.txt" ] &&
  cp "$ORTROOT/ThirdPartyNotices.txt" "$dir/licenses/onnxruntime-third-party-notices.txt"
(cd "$ROOT" && go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}' ./cmd/duetty) |
  sort -u | while read -r mod moddir; do
    lic="$(find "$moddir" -maxdepth 1 -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' -o -iname 'UNLICENSE*' \) -print -quit)"
    if [ -z "$lic" ]; then
      echo "error: no license file found for Go module $mod" >&2
      exit 1
    fi
    cp "$lic" "$dir/licenses/go/$(echo "$mod" | tr '/' '_').txt"
  done

"$dir/duetty" -h >/dev/null # smoke test: the binary starts and prints its help

mkdir -p "$OUT"
tar -C "$stage" -czf "$OUT/$NAME.tar.gz" "$NAME"
(cd "$OUT" && { command -v sha256sum >/dev/null && sha256sum "$NAME.tar.gz" || shasum -a 256 "$NAME.tar.gz"; } > "$NAME.tar.gz.sha256")
echo "Wrote $OUT/$NAME.tar.gz"
