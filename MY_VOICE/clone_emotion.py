#!/usr/bin/env python3
"""Emotion-routed voice cloning.

Takes a JSON file mapping text chunks to emotions, generates each chunk
with the matching cached voice prompt, and stitches the results into
one continuous WAV.

Usage:
    python clone_emotion.py --script chapter.json --output chapter.wav

chapter.json format:
[
    {"text": "I walked in. Everything looked the same.", "emotion": "neutral"},
    {"text": "Then I saw the photo on the table.", "emotion": "surprised"},
    {"text": "It was the last one he ever took.", "emotion": "sad"},
    {"text": "I threw the book across the room.", "emotion": "angry"}
]

Emotion keys must match the prompt files in --prompts-dir:
    neutral_prompt.pt, surprised_prompt.pt, sad_prompt.pt, angry_prompt.pt
"""

import argparse
import json
import os
import re
import subprocess
import tempfile
import torch
import soundfile as sf
from qwen_tts import Qwen3TTSModel


def split_sentences(text: str) -> list[str]:
    """Split text into sentence-like chunks. Keeps chunks short enough
    for the model to maintain prosody consistently."""
    # Split on sentence boundaries, but keep chunks under ~200 chars
    # by also splitting on commas/semicolons for long sentences.
    sentences = re.split(r'(?<=[.!?])\s+', text)
    chunks = []
    for s in sentences:
        s = s.strip()
        if not s:
            continue
        # If a "sentence" is very long, split on commas/semicolons
        if len(s) > 200:
            parts = re.split(r'(?<=[,;])\s+', s)
            buffer = ""
            for p in parts:
                if buffer and len(buffer) + len(p) + 1 > 150:
                    chunks.append(buffer)
                    buffer = p
                else:
                    buffer = f"{buffer}, {p}" if buffer else p
            if buffer:
                chunks.append(buffer)
        else:
            chunks.append(s)
    return chunks


def main():
    parser = argparse.ArgumentParser(description="Emotion-routed voice cloning")
    parser.add_argument("--script", required=True, help="JSON file: list of {text, emotion} objects")
    parser.add_argument("--output", required=True, help="Output WAV path")
    parser.add_argument("--prompts-dir", default=".", help="Directory containing <emotion>_prompt.pt files")
    parser.add_argument("--model", default="Qwen/Qwen3-TTS-12Hz-0.6B-Base", help="Model name")
    parser.add_argument("--device", default="cuda:0", help="Device (default: cuda:0)")
    parser.add_argument("--language", default="English", help="Target language")
    parser.add_argument("--gap-ms", type=int, default=150, help="Silence gap between chunks in ms (default 150)")
    args = parser.parse_args()

    # Load script
    with open(args.script) as f:
        entries = json.load(f)

    if not entries:
        print("Error: script is empty")
        exit(1)

    # Load model
    print(f"Loading {args.model} on {args.device}...")
    model = Qwen3TTSModel.from_pretrained(
        args.model,
        device_map=args.device,
        dtype=torch.bfloat16,
        attn_implementation="flash_attention_2",
    )

    # Load all needed prompts
    emotions = list(dict.fromkeys(e["emotion"] for e in entries))  # preserve order, dedupe
    prompts = {}
    for emo in emotions:
        path = os.path.join(args.prompts_dir, f"{emo}_prompt.pt")
        if not os.path.isfile(path):
            print(f"Error: missing prompt file: {path}")
            print(f"  Run: python clone_voice.py --ref <{emo}.wav> --ref-text \"<transcript>\" "
                  f"--text test --output /dev/null --cache-file {path} --save-cache")
            exit(1)
        prompts[emo] = torch.load(path, map_location=args.device)
        print(f"  Loaded: {path}")

    # Generate each chunk
    print(f"\nGenerating {len(entries)} chunk(s) across {len(emotions)} emotion(s)...")
    all_wavs = []
    for i, entry in enumerate(entries):
        text = entry["text"]
        emo = entry["emotion"]
        print(f"  [{i+1}/{len(entries)}] ({emo}) {text[:60]}{'...' if len(text) > 60 else ''}")
        wavs, sr = model.generate_voice_clone(
            text=[text],
            language=[args.language],
            voice_clone_prompt=prompts[emo],
        )
        all_wavs.append(wavs[0])

    # Build silence gap
    gap_samples = int(sr * args.gap_ms / 1000)
    silence = torch.zeros(gap_samples, dtype=all_wavs[0].dtype)

    # Concatenate with gaps
    segments = []
    for i, wav in enumerate(all_wavs):
        segments.append(wav)
        if i < len(all_wavs) - 1:
            segments.append(silence)

    final = torch.cat(segments, dim=0)
    sf.write(args.output, final.cpu().numpy(), sr)
    print(f"\nSaved: {args.output} ({len(final) / sr:.1f}s)")


if __name__ == "__main__":
    main()
