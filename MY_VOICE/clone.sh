#!/usr/bin/env bash
# Quick voice clone wrapper.
# Usage: ./clone.sh "text to say" [output.wav]
#
# First-time setup:
#   ./clone.sh --init          # builds the cached prompt from my_voice.wav + ref_text.txt
#
# After that:
#   ./clone.sh "Hello, this is my cloned voice."
#
# Expects my_voice.wav and ref_text.txt in this directory (for --init only).

set -euo pipefail

REF="my_voice.wav"
REF_TEXT_FILE="ref_text.txt"
CACHE_FILE="voice_prompt.pt"
MODEL="Qwen/Qwen3-TTS-12Hz-0.6B-Base"
DEVICE="cuda:0"

# --- First-time init: build the cached prompt ---
if [[ "${1:-}" == "--init" ]]; then
    if [[ ! -f "$REF" ]]; then
        echo "Error: $REF not found. Record a reference first."
        echo "  ffmpeg -i recording.mp3 -ar 16000 -ac 1 $REF"
        exit 1
    fi
    if [[ ! -f "$REF_TEXT_FILE" ]]; then
        echo "Error: $REF_TEXT_FILE not found. Write your transcript there."
        exit 1
    fi
    REF_TEXT=$(cat "$REF_TEXT_FILE")
    echo "Building voice prompt from $REF..."
    python clone_voice.py \
        --ref "$REF" \
        --ref-text "$REF_TEXT" \
        --text "init" \
        --output /dev/null \
        --cache-file "$CACHE_FILE" \
        --save-cache \
        --model "$MODEL" \
        --device "$DEVICE"
    echo "Done. Prompt cached to $CACHE_FILE."
    echo "Now just run: $0 \"your text here\""
    exit 0
fi

# --- Normal clone: use cached prompt ---
if [[ $# -lt 1 ]]; then
    echo "Usage:"
    echo "  $0 --init                    # one-time: build cached prompt"
    echo "  $0 \"text to say\" [output]   # clone using cached prompt"
    exit 1
fi

if [[ ! -f "$CACHE_FILE" ]]; then
    echo "Error: $CACHE_FILE not found. Run '$0 --init' first."
    exit 1
fi

TEXT="$1"
OUTPUT="${2:-output_$(date +%Y%m%d_%H%M%S).wav}"

echo "Cloning voice..."
echo "  prompt: $CACHE_FILE"
echo "  text:   $TEXT"
echo "  output: $OUTPUT"

python clone_voice.py \
    --text "$TEXT" \
    --output "$OUTPUT" \
    --cache-file "$CACHE_FILE" \
    --model "$MODEL" \
    --device "$DEVICE"

echo "Done: $OUTPUT"
