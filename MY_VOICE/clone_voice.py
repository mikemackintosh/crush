#!/usr/bin/env python3
"""Qwen3-TTS voice clone script.

Usage:
    python clone_voice.py --ref ref.wav --ref-text "transcript" --text "What to say"
    python clone_voice.py --text "What to say" --cache-file my_voice_prompt.pt
"""

import argparse
import json
import os
import torch
import soundfile as sf
from qwen_tts import Qwen3TTSModel


def main():
    parser = argparse.ArgumentParser(description="Clone a voice with Qwen3-TTS")
    parser.add_argument("--ref", help="Path to reference audio (WAV, 16kHz mono, 10-15s)")
    parser.add_argument("--ref-text", help="Exact transcript of the reference audio")
    parser.add_argument("--text", required=True, help="Text to synthesize (plain string or JSON list)")
    parser.add_argument("--language", default="English", help="Target language (default: English)")
    parser.add_argument("--output", default="output.wav", help="Output WAV path (or prefix for multiple)")
    parser.add_argument("--model", default="Qwen/Qwen3-TTS-12Hz-0.6B-Base", help="Model name")
    parser.add_argument("--device", default="cuda:0", help="Device (default: cuda:0)")
    parser.add_argument("--cache-file", default="voice_prompt.pt", help="Path to cached voice prompt")
    parser.add_argument("--save-cache", action="store_true", help="Save the voice prompt to --cache-file")
    parser.add_argument("--x-vector", action="store_true", help="Use x-vector-only mode (faster, lower fidelity)")
    args = parser.parse_args()

    # Parse text: plain string or JSON list
    try:
        texts = json.loads(args.text)
        if not isinstance(texts, list):
            texts = [str(texts)]
    except json.JSONDecodeError:
        texts = [args.text]

    # Load model
    print(f"Loading {args.model} on {args.device}...")
    model = Qwen3TTSModel.from_pretrained(
        args.model,
        device_map=args.device,
        dtype=torch.bfloat16,
        attn_implementation="flash_attention_2",
    )

    # Resolve voice prompt: cached file > fresh extraction
    prompt_items = None
    if os.path.isfile(args.cache_file):
        print(f"Loading cached prompt from {args.cache_file}...")
        prompt_items = torch.load(args.cache_file, map_location=args.device)
    elif args.ref and args.ref_text:
        print("Extracting voice prompt from reference...")
        prompt_items = model.create_voice_clone_prompt(
            ref_audio=args.ref,
            ref_text=args.ref_text,
            x_vector_only_mode=args.x_vector,
        )
        if args.save_cache:
            torch.save(prompt_items, args.cache_file)
            print(f"Prompt saved to {args.cache_file}")
    else:
        print("Error: need either --cache-file (existing) or --ref + --ref-text")
        exit(1)

    # Generate
    print(f"Generating {len(texts)} sentence(s)...")
    wavs, sr = model.generate_voice_clone(
        text=texts,
        language=[args.language] * len(texts),
        voice_clone_prompt=prompt_items,
    )

    # Save
    if len(wavs) > 1:
        for i, wav in enumerate(wavs):
            out = f"{args.output}_{i}.wav"
            sf.write(out, wav, sr)
            print(f"Saved: {out}")
    else:
        sf.write(args.output, wavs[0], sr)
        print(f"Saved: {args.output}")


if __name__ == "__main__":
    main()
