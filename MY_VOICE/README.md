# MY_VOICE — Qwen3-TTS Voice Cloning

Self-hosted voice cloning with Qwen3-TTS. Record a 10–15s sample of your voice, cache the prompt once, then generate speech near-instantly.

## Prerequisites

```bash
pip install qwen-tts torch soundfile
# For flash attention (recommended):
pip install flash-attn
```

Requires a CUDA GPU with enough VRAM (~2 GB for the 0.6B model in bf16, ~4 GB for 1.7B).

## Quick Start

```bash
cd MY_VOICE

# 1. Record 10-15s of clean speech, convert to 16kHz mono WAV:
ffmpeg -i recording.mp3 -ar 16000 -ac 1 my_voice.wav

# 2. Write the exact transcript to ref_text.txt:
echo "This is exactly what I said in the recording." > ref_text.txt

# 3. One-time init: build and cache the voice prompt
./clone.sh --init

# 4. Clone: just pass text, get WAV
./clone.sh "Hello, this is my cloned voice."
# -> output_20250715_143022.wav
```

## How It Works (Fast Path)

The script uses a **two-phase** approach:

1. **Init (once):** `create_voice_clone_prompt()` encodes your reference audio + transcript into a compact prompt (~5–10 KB), saved to `voice_prompt.pt`.
2. **Generate (every call):** `generate_voice_clone()` loads the cached prompt and skips all reference encoding. This is the 30–50% speedup.

## Speed Tiers

| Tier | Model | Mode | Use case |
|---|---|---|---|
| Fast | `0.6B-Base` | `--x-vector` | Quick identity-only clone, lowest latency |
| Balanced | `0.6B-Base` | ICL (default) | Good fidelity, fast |
| High fidelity | `1.7B-Base` | ICL (default) | Best quality, slower |

Switch model with `--model` flag or edit the `MODEL` variable in `clone.sh`.

## CLI Options

| Flag | Default | Description |
|---|---|---|
| `--text` | (required) | Text to synthesize (string or JSON list) |
| `--cache-file` | `voice_prompt.pt` | Path to cached voice prompt |
| `--ref` | — | Reference WAV (only needed for init) |
| `--ref-text` | — | Transcript of reference (only needed for init) |
| `--save-cache` | off | Save prompt to `--cache-file` |
| `--x-vector` | off | Speaker-embedding only (faster, lower fidelity) |
| `--language` | `English` | Target language |
| `--output` | `output.wav` | Output path or prefix |
| `--model` | `Qwen/Qwen3-TTS-12Hz-0.6B-Base` | Model to use |
| `--device` | `cuda:0` | GPU device |

## Multiple Voices

Cache a prompt per voice:

```bash
python clone_voice.py \
  --ref dad.wav --ref-text "Dad's transcript" \
  --text "test" --output /dev/null \
  --cache-file dad_prompt.pt --save-cache

python clone_voice.py \
  --ref kid.wav --ref-text "Kid's transcript" \
  --text "test" --output /dev/null \
  --cache-file kid_prompt.pt --save-cache
```

Then generate with any of them:

```bash
python clone_voice.py --text "Hello" --cache-file dad_prompt.pt --output dad.wav
```

## Tips

- **Match the style**: record calm narration for calm output; energetic speech for energetic output
- **Transcript accuracy matters**: mismatched `--ref-text` degrades clone quality
- **Test short first**: generate one sentence before committing to long content
- **Batch**: pass a JSON list to `--text` for offline/queue workloads (audiobooks, many lines)

## Getting Emotion and Inflection

The clone mirrors the **style** of your reference clip. The model doesn't have a "happy" or "sad" dial — it reproduces the prosody (pitch contour, pace, energy, micro-pauses) baked into the audio you feed it.

### How emotion transfer works

Qwen3-TTS operates in two modes:

| Mode | What it captures | Emotion fidelity |
|---|---|---|
| **ICL** (default, uses `ref_text`) | Speaker identity + prosody + paralinguistic context (pitch variation, rhythm, emotional coloring) | High — the model "hears" how you said it, not just what you said |
| **X-Vector** (`--x-vector`) | Speaker embedding only (timbre, pitch range) | Low — you get your voice but lose the emotional delivery |

**Bottom line:** if you want emotion, use ICL mode (the default) and record your reference with the emotion you want.

### Recording references for specific emotions

Record **separate clips** for different emotional registers, then cache a prompt for each:

```bash
# Neutral / conversational
./clone.sh --init  # uses my_voice.wav + ref_text.txt (default)

# Excited / high energy
python clone_voice.py \
  --ref excited.wav --ref-text "transcript of the excited clip" \
  --text "test" --output /dev/null \
  --cache-file excited_prompt.pt --save-cache

# Sad / reflective
python clone_voice.py \
  --ref sad.wav --ref-text "transcript of the sad clip" \
  --text "test" --output /dev/null \
  --cache-file sad_prompt.pt --save-cache

# Angry / intense
python clone_voice.py \
  --ref angry.wav --ref-text "transcript of the angry clip" \
  --text "test" --output /dev/null \
  --cache-file angry_prompt.pt --save-cache
```

Then pick the emotional register per generation:

```bash
# Neutral
python clone_voice.py --text "I'll be there at five." --cache-file voice_prompt.pt --output neutral.wav

# Excited
python clone_voice.py --text "I'll be there at five!" --cache-file excited_prompt.pt --output excited.wav

# Sad
python clone_voice.py --text "I'll be there at five." --cache-file sad_prompt.pt --output sad.wav
```

### What to record for each emotion

| Emotion | How to perform it | Key prosody features |
|---|---|---|
| **Neutral** | Casual conversation, steady pace | Flat pitch contour, natural micro-pauses |
| **Happy / Excited** | Smiling while talking, slightly faster, raised pitch | Upward pitch contour, faster syllable rate, brighter timbre |
| **Sad / Reflective** | Slower pace, lower volume, downward pitch | Downward pitch contour, longer pauses, softer attack |
| **Angry / Intense** | Faster, louder, harder consonants, shorter pauses | High energy, sharp plosives, compressed rhythm |
| **Calm / Soothing** | Slow, even, low volume, long exhales | Low pitch range, smooth transitions, minimal pitch variation |
| **Surprised** | Quick pitch jump at the start, then settle | Initial pitch spike, slightly faster, open vowels |

### Recording tips for emotional references

1. **Act it, don't just say it.** If you want a sad clip, actually feel the sadness for those 10–15 seconds. The model captures the delivery, not the words.
2. **One emotion per clip.** Don't mix a happy opening with a sad ending in the same reference. The model will average them.
3. **Keep the transcript accurate.** Even for emotional clips, `ref_text` must match the audio exactly.
4. **Match the text to the emotion.** A sad reference works best when the generated text is also sad in content. "I'll be there at five" from a sad reference will sound oddly mournful for a neutral statement.
5. **10–15 seconds is enough.** You don't need a long monologue. A single emotional sentence, delivered well, is all the model needs.
6. **Record in a quiet room.** Emotional delivery often involves volume changes; background noise gets amplified in the quiet parts.

### Mixing emotions in a single generation

The model generates one emotional register per call (set by the cached prompt). For a line that shifts emotion mid-sentence, you have two options:

- **Split and stitch:** generate the first half with prompt A, the second half with prompt B, then join the WAVs:
  ```bash
  python clone_voice.py --text "I thought everything was fine." --cache-file neutral_prompt.pt --output part1.wav
  python clone_voice.py --text "Then I saw the email." --cache-file surprised_prompt.pt --output part2.wav
  ffmpeg -i part1.wav -i part2.wav -filter_complex "[0:a][1:a]concat=n=2:v=0:a=1" stitched.wav
  ```
- **Use a reference that contains the shift:** record a 15s clip where you naturally transition from calm to surprised. The model will reproduce that arc.

### What the model can't do

- **It won't add emotion you didn't record.** A neutral reference produces neutral output, even if the text is "I'M SO HAPPY!"
- **It won't sustain one emotion across a 5-minute audiobook.** The prosody will drift toward the reference's average register. For long-form, generate in 2–3 sentence chunks and pick the best take.
- **It can't do singing, whispering, or shouting well.** These extreme registers degrade the speaker embedding. Record within your normal speaking range.

## Troubleshooting

- **`flash_attention_2` error**: install `flash-attn` or change `attn_implementation` to `"sdpa"` in the script
- **Out of memory**: use the 0.6B model (default) or set `--device cpu` (much slower)
- **No CUDA**: set `--device cpu` (works, but slow)
