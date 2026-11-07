#!/usr/bin/env bash
# Installs the optional denoise model (config `denoise = true` / -denoise)
# under models/denoise/ (gitignored). Re-running is safe: a present model with
# the right checksum is kept. Requires the onnxruntime library from
# scripts/setup-models.sh.
#
# Model: GTCRN, "GTCRN: A Speech Enhancement Model Requiring Ultralow
# Computational Resources" (Rong et al., ICASSP 2024), streaming ONNX export
# stream/onnx_models/gtcrn_simple.onnx from https://github.com/Xiaobin-Rong/gtcrn
# (MIT license), pinned to commit 502ebfab64da7c4a9af78dcb9c6ceef1ebb01c73.
# ~48k parameters, 16 kHz, trained on DNS3; inputs one 512-point STFT frame
# (256 hop, sqrt-Hann) plus three cache tensors, which internal/denoise drives
# from Go (with 48k<->16k resampling around it).
#
# Why not DeepFilterNet3 (48 kHz, preferred): there is no official
# single-graph streaming ONNX. The official DeepFilterNet3_onnx.tar.gz is three
# graphs (enc / erb_dec / df_dec) that need ERB + unit-norm feature extraction
# and multi-frame deep filtering outside the graph; the torchDF streaming
# export (grazder/DeepFilterNet, torchDF-changes) ships no prebuilt model and
# needs a torch + Rust build to export; third-party per-frame exports on
# Hugging Face use project-specific contracts. GTCRN's official streaming
# export is self-contained apart from a plain STFT, and Silero and Whisper
# consume 16 kHz anyway, so the lower rate costs nothing downstream.
set -euo pipefail

GTCRN_REV="${GTCRN_REV:-502ebfab64da7c4a9af78dcb9c6ceef1ebb01c73}"
GTCRN_URL="${GTCRN_URL:-https://raw.githubusercontent.com/Xiaobin-Rong/gtcrn/${GTCRN_REV}/stream/onnx_models/gtcrn_simple.onnx}"
GTCRN_SHA256="${GTCRN_SHA256:-b4718df6228e7bdf1a8a435cf98f838636eb2fd331acabf86ba87c5192ebcb87}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

require() { command -v "$1" >/dev/null 2>&1 || { echo "error: '$1' is required but not installed" >&2; exit 1; }; }

fetch() {
  local url="$1" dest="$2" tmp
  tmp="$(mktemp "${dest}.XXXXXX")"
  if ! curl -fSL --retry 3 --retry-delay 2 -o "$tmp" "$url"; then
    rm -f "$tmp"; echo "error: failed to download $url" >&2; exit 1
  fi
  mv -f "$tmp" "$dest"
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

mkdir -p "$ROOT/models/denoise"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

DN="$ROOT/models/denoise/gtcrn_simple.onnx"
if [ -f "$DN" ] && [ "$(sha256_of "$DN")" = "$GTCRN_SHA256" ]; then
  echo "denoise model already present, skipping."
else
  require curl
  echo "Downloading GTCRN denoise model..."
  fetch "$GTCRN_URL" "$tmp/gtcrn_simple.onnx"
  got="$(sha256_of "$tmp/gtcrn_simple.onnx")"
  [ "$got" = "$GTCRN_SHA256" ] || { echo "error: denoise model SHA-256 mismatch (got $got)" >&2; exit 1; }
  # Atomic install: temp file in the target directory, then mv.
  t="$(mktemp "$ROOT/models/denoise/.gtcrn_simple.onnx.XXXXXX")"
  if ! cp "$tmp/gtcrn_simple.onnx" "$t"; then rm -f "$t"; echo "error: cannot write $DN" >&2; exit 1; fi
  chmod 644 "$t"
  mv -f "$t" "$DN"
fi

cat <<EOF
Done:
  models/denoise/gtcrn_simple.onnx
Enable with \`denoise = true\` in the config or -denoise.
EOF
