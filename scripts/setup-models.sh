#!/usr/bin/env bash
# Installs the models and onnxruntime duetty needs under models/ and
# third_party/ (both gitignored): Silero VAD, Smart Turn v3.2 and the
# onnxruntime shared library. If a talktome checkout is present
# (TALKTOME_DIR, default ~/llmchat/talktome) its copies are used; otherwise
# everything is downloaded. Re-running is safe: present assets are kept.
# Every download is pinned and SHA-256 checked; every install is atomic (temp
# file in the target directory, then mv).
set -euo pipefail

ORT_VERSION="${ORT_VERSION:-1.27.0}"
# ORT_SHA256 overrides the per-platform checksum below (required when
# ORT_VERSION is changed).
ORT_SHA256="${ORT_SHA256:-}"
# Silero VAD v6.2 model, pinned to release tag v6.2.3 (file last changed in
# commit bfdc0193023f121ea5b3cc7b176dbed570a68a59, "add v6.2 model").
SILERO_REV="${SILERO_REV:-v6.2.3}"
SILERO_VAD_MODEL_URL="${SILERO_VAD_MODEL_URL:-https://raw.githubusercontent.com/snakers4/silero-vad/${SILERO_REV}/src/silero_vad/data/silero_vad.onnx}"
SILERO_SHA256="${SILERO_SHA256:-1a153a22f4509e292a94e67d6f9b85e8deb25b4988682b7e174c65279d8788e3}"
SMART_TURN_REV="${SMART_TURN_REV:-f766f81d3cfd}"
SMART_TURN_URL="${SMART_TURN_URL:-https://huggingface.co/pipecat-ai/smart-turn-v3/resolve/${SMART_TURN_REV}/smart-turn-v3.2-cpu.onnx}"
SMART_TURN_SHA256="${SMART_TURN_SHA256:-2bb026316b14a660486a75b1733cd3fbab8c2fd0314dc9af7be49f8cca967e4f}"
TALKTOME_DIR="${TALKTOME_DIR:-$HOME/llmchat/talktome}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

require() { command -v "$1" >/dev/null 2>&1 || { echo "error: '$1' is required but not installed" >&2; exit 1; }; }

ort_dir() {
  case "$(uname -s)-$(uname -m)" in
    Linux-x86_64) echo "onnxruntime-linux-x64-${ORT_VERSION}" ;;
    Linux-aarch64|Linux-arm64) echo "onnxruntime-linux-aarch64-${ORT_VERSION}" ;;
    Darwin-arm64) echo "onnxruntime-osx-arm64-${ORT_VERSION}" ;;
    *) echo "error: no onnxruntime v$ORT_VERSION release for $(uname -s)/$(uname -m)" >&2; exit 1 ;;
  esac
}

# ort_sha256 DIR: the release tarball checksum (GitHub's published asset
# digests for v1.27.0), or $ORT_SHA256.
ort_sha256() {
  if [ -n "$ORT_SHA256" ]; then echo "$ORT_SHA256"; return; fi
  case "$1" in
    onnxruntime-linux-x64-1.27.0) echo 547e40a48f1fe73e3f812d7c88a948612c23f896b91e4e2ee1e232d7b468246f ;;
    onnxruntime-linux-aarch64-1.27.0) echo 3e4d83ac06924a32a07b6d7f91ce6f852876153fc0bbdf931bf517a140bfbe48 ;;
    onnxruntime-osx-arm64-1.27.0) echo 545e81c58152353acb0d1e8bd6ce4b62f830c0961f5b3acfedc790ffd76e477a ;;
    *) echo "error: no pinned checksum for $1; set ORT_SHA256" >&2; exit 1 ;;
  esac
}

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

# check FILE SHA256 WHAT: exit unless FILE has the expected checksum.
check() {
  local got
  got="$(sha256_of "$1")"
  [ "$got" = "$2" ] || { echo "error: $3 SHA-256 mismatch (got $got)" >&2; exit 1; }
}

# install_file SRC DEST: atomic copy (temp file next to DEST, then mv).
install_file() {
  local t
  t="$(mktemp "$(dirname "$2")/.$(basename "$2").XXXXXX")"
  if ! cp "$1" "$t"; then rm -f "$t"; echo "error: cannot copy to $2" >&2; exit 1; fi
  chmod 644 "$t"
  mv -f "$t" "$2"
}

mkdir -p "$ROOT/models/smart-turn" "$ROOT/third_party/onnxruntime"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --- Silero VAD ---
SILERO="$ROOT/models/silero_vad.onnx"
TT_SILERO="$TALKTOME_DIR/models/silero_vad.onnx"
if [ -f "$SILERO" ] && [ "$(sha256_of "$SILERO")" = "$SILERO_SHA256" ]; then
  echo "silero-vad model already present, skipping."
elif [ -f "$TT_SILERO" ] && [ "$(sha256_of "$TT_SILERO")" = "$SILERO_SHA256" ]; then
  echo "Copying Silero VAD model from $TALKTOME_DIR..."
  install_file "$TT_SILERO" "$SILERO"
else
  require curl
  echo "Downloading Silero VAD model ($SILERO_REV)..."
  fetch "$SILERO_VAD_MODEL_URL" "$tmp/silero_vad.onnx"
  check "$tmp/silero_vad.onnx" "$SILERO_SHA256" "silero-vad model"
  install_file "$tmp/silero_vad.onnx" "$SILERO"
fi

# --- Smart Turn ---
ST="$ROOT/models/smart-turn/smart-turn-v3.2-cpu.onnx"
TT_ST="$TALKTOME_DIR/models/smart-turn/smart-turn-v3.2-cpu.onnx"
if [ -f "$ST" ] && [ "$(sha256_of "$ST")" = "$SMART_TURN_SHA256" ]; then
  echo "smart-turn model already present, skipping."
elif [ -f "$TT_ST" ] && [ "$(sha256_of "$TT_ST")" = "$SMART_TURN_SHA256" ]; then
  echo "Copying Smart Turn model from $TALKTOME_DIR..."
  install_file "$TT_ST" "$ST"
else
  require curl
  echo "Downloading Smart Turn v3.2 model..."
  fetch "$SMART_TURN_URL" "$tmp/smart-turn.onnx"
  check "$tmp/smart-turn.onnx" "$SMART_TURN_SHA256" "smart-turn model"
  install_file "$tmp/smart-turn.onnx" "$ST"
fi

# --- onnxruntime ---
# The library directory is staged next to its final place and moved in whole,
# so an interrupted run never leaves a half-copied lib/ that looks installed.
ORTROOT="$ROOT/third_party/onnxruntime"
LIBDIR="$ORTROOT/lib"
link_so() { # link_so DIR: make DIR/libonnxruntime.so point at the versioned library
  if [ ! -e "$1/libonnxruntime.so" ]; then
    local so
    so="$(find "$1" \( -name 'libonnxruntime.so.*' -o -name 'libonnxruntime.dylib' -o -name 'libonnxruntime.*.dylib' \) -print -quit)"
    [ -n "$so" ] && ln -sf "$(basename "$so")" "$1/libonnxruntime.so"
  fi
}
install_libdir() { # install_libdir SRC: atomically replace LIBDIR with a copy of SRC
  local stage old
  stage="$(mktemp -d "$ORTROOT/.lib.XXXXXX")"
  cp -a "$1/." "$stage/" || { rm -rf "$stage"; echo "error: cannot copy onnxruntime" >&2; exit 1; }
  link_so "$stage"
  [ -e "$stage/libonnxruntime.so" ] || { rm -rf "$stage"; echo "error: libonnxruntime.so missing" >&2; exit 1; }
  chmod 755 "$stage"
  if [ -e "$LIBDIR" ]; then
    old="$(mktemp -d "$ORTROOT/.lib-old.XXXXXX")"
    mv "$LIBDIR" "$old/lib"
    mv "$stage" "$LIBDIR"
    rm -rf "$old"
  else
    mv "$stage" "$LIBDIR"
  fi
}
if [ -e "$LIBDIR/libonnxruntime.so" ] || ls "$LIBDIR"/libonnxruntime.so.* >/dev/null 2>&1; then
  echo "onnxruntime library already present, skipping."
  link_so "$LIBDIR"
elif ls "$TALKTOME_DIR"/third_party/onnxruntime/lib/libonnxruntime.so.* >/dev/null 2>&1; then
  echo "Copying onnxruntime from $TALKTOME_DIR..."
  install_libdir "$TALKTOME_DIR/third_party/onnxruntime/lib"
else
  require curl; require tar
  ORT_DIR="$(ort_dir)"
  ORT_SUM="$(ort_sha256 "$ORT_DIR")"
  echo "Downloading onnxruntime v$ORT_VERSION ($ORT_DIR)..."
  fetch "https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}/${ORT_DIR}.tgz" "$tmp/onnxruntime.tgz"
  check "$tmp/onnxruntime.tgz" "$ORT_SUM" "onnxruntime tarball"
  tar xzf "$tmp/onnxruntime.tgz" -C "$tmp"
  install_libdir "$tmp/$ORT_DIR/lib"
fi
[ -e "$LIBDIR/libonnxruntime.so" ] || { echo "error: libonnxruntime.so missing" >&2; exit 1; }

cat <<EOF
Done:
  models/silero_vad.onnx
  models/smart-turn/smart-turn-v3.2-cpu.onnx
  third_party/onnxruntime/lib/libonnxruntime.so
EOF
