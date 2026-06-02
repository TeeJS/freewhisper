# FreeWhisper

A Wispr Flow alternative for Windows. Push-to-talk dictation that captures mic audio, sends it to a self-hosted faster-whisper server, and pastes the transcribed text into the active window.

## Goals

- **Single self-contained `.exe`** — no installer, no runtime dependencies, no Python.
- **Low EDR profile** — the work laptop runs FortiEDR. The keyboard-hook + mic + outbound + paste behavior pattern is unavoidable, but everything else should look as normal as possible. No PyInstaller-style runtime extraction. Native compiled Go.
- **Talks to a remote whisper endpoint** on the user's home LAN (RFC1918 address). Audio capture happens on the laptop; transcription happens on the home GPU box.
- **Push-to-talk ergonomics** — hold hotkey, speak, release, text appears at cursor. Same flow as Wispr Flow / WhisperType / SuperWhisper.

## Non-goals

- Local whisper inference on the laptop. Server does all the ML work.
- Cross-platform. Windows only. macOS/Linux are out of scope.
- Cloud fallback (Groq, OpenAI). Local server only.
- Meeting transcription, diarization, AI cleanup, agent mode. Just dictation.
- Code signing or distribution. This is a personal tool, not a product.

## Architecture

```
[hotkey pressed]
       ↓
[WASAPI mic capture starts → PCM bytes in memory]
       ↓
[VAD splits the stream into chunks at speech pauses]   ← streaming: happens while held
       ↓
[each chunk → Wyoming-protocol TCP send to whisper endpoint (transcribes in parallel)]
       ↓
[ordered paste queue reassembles transcripts in sequence]
       ↓
[clipboard SetText + SendInput Ctrl+V into active window, in order]
       ↓
[hotkey released → final chunk flushed, queue drains]
       ↓
[idle, wait for next hotkey press]
```

The original design was batch (record-all-then-POST over OpenAI-compatible
HTTP). It's since become **streaming over the Wyoming protocol**: a VAD
(`vad.go`) cuts the audio into utterance-sized chunks at natural pauses, each
is transcribed as it's ready, and a sequence-numbered paste queue keeps the
output in order even when chunks finish out of order. See "Whisper endpoint"
below.

## File layout

```
freewhisper/
├── CLAUDE.md           ← this file
├── README.md           ← user-facing setup/usage docs
├── go.mod
├── go.sum
├── main.go             ← entry point, tray icon, hotkey wiring, streaming consumer, ordered paster
├── recorder.go         ← WASAPI mic capture, VAD-bounded ChunkedRecorder, pre-roll ring, WAV writer
├── vad.go              ← energy-based Voice Activity Detector (with ambient-noise calibration)
├── transcriber.go      ← Wyoming-protocol TCP client (JSONL + binary PCM)
├── paster.go           ← clipboard write + Ctrl-state-aware SendInput Ctrl+V
├── indicators.go       ← tray-icon color swap + Beep() during recording
├── settings.go         ← walk-based settings dialog (right-click → Settings…)
├── hotkeymap.go        ← string ↔ hotkey.Modifier/Key lookups (shared by config + GUI)
├── config.go           ← Config struct, Load/Save, defaults, first-run detection
├── config.example.json ← committed example config (placeholder host/port)
├── icon.ico            ← idle tray icon (blue)
├── icon_recording.ico  ← recording tray icon (red, shown when NotifyColorChange on)
├── freewhisper.exe.manifest ← Win32 manifest source (Common Controls v6, DPI awareness)
└── manifest.syso       ← compiled manifest resource (embedded by `go build`)
```

There is no build script; build with the `go build` command under "Build" below.

`config.json` is gitignored. `config.example.json` is committed.

## Key dependencies

- `github.com/getlantern/systray` — tray icon
- `golang.design/x/hotkey` — global hotkey registration
- `github.com/go-ole/go-ole` + `github.com/moutend/go-wca` — WASAPI mic capture (COM wrappers)
- `golang.org/x/sys/windows` — official Windows syscall bindings, used for SendInput and clipboard
- `github.com/lxn/walk` + `github.com/lxn/walk/declarative` — Win32 GUI for the settings dialog

All MIT/BSD. No exotic transitive dependencies expected.

**Manifest regeneration:** if you edit `freewhisper.exe.manifest`, regenerate the embedded `.syso` with:

```powershell
go install github.com/akavel/rsrc@latest
rsrc -manifest freewhisper.exe.manifest -o manifest.syso -arch amd64
```

`go build` picks up `manifest.syso` automatically because of its `.syso` extension. The manifest is required for walk's modern Common Controls (v6) — without it, settings-dialog tooltip creation panics.

## Whisper endpoint

The home faster-whisper server speaks the **Wyoming protocol** (not OpenAI-compatible HTTP — that was an early assumption that turned out wrong). Wyoming is the same protocol used by Home Assistant's voice pipeline and Rhasspy — JSONL header lines plus binary audio payloads over plain TCP.

Default endpoint: `192.168.1.25:10300` (standard Wyoming whisper port). Server reports `faster-whisper 3.1.0` with the `medium` model.

`transcriber.go` implements a minimal Wyoming client from scratch — no external library needed. The dance is:

1. `transcribe` event (language hint)
2. `audio-start` (sample rate, width, channels)
3. `audio-chunk` × N (binary PCM payload, ~50 ms each)
4. `audio-stop`
5. read `transcript` event from server

The server resamples our 48 kHz capture to 16 kHz internally, so no client-side resampling is needed. Endpoint host/port and language live in `config.json`.

## User context

The maintainer (T.J.) is an experienced infrastructure/sysadmin (Windows domain, Docker, home lab, self-hosted AI stack) but **new to most programming**. Treat this as a learning project alongside a delivery project:

- Explain *why*, not just *what*, when making non-obvious choices.
- Prefer clear, idiomatic Go over clever Go.
- Comment generously — especially around Windows API calls and COM, which are foreign to most newcomers.
- One concept at a time. Don't dump three new patterns into a single commit.
- When stuck, do real research (docs, GitHub issues) before guessing. Three failed attempts means stop and read.

## Conventions

- **Always create a timestamped backup before modifying any file**, unless the file is under git version control with a clean working tree (in which case the commit history is the backup). When in doubt, back up.
- **Commit early, commit often.** Every working state gets a commit. The git log is the development journal.
- **No unrequested changes.** If you notice something else worth fixing, mention it, don't silently fix it.
- **Pushback protocol.** If the user pushes back on a suggestion, take the concern at face value. Stop and rework from their constraints — do not re-explain or defend.
- **No `configuration.yaml`-style sprawl.** Configuration lives in `config.json`. Code lives in `.go` files. Don't invent a third place.

## Build

```powershell
# From repo root
go build -ldflags="-H windowsgui -s -w" -o freewhisper.exe
```

- `-H windowsgui` suppresses the console window (this is a tray app, not a CLI).
- `-s -w` strips debug symbols, reduces binary size.

Expected binary size: ~8-12 MB.

## Testing

Manual for now. The full loop (hotkey → speak → text appears) is the integration test. Unit tests for the WAV header construction and JSON parsing are worth writing once those modules stabilize.

## Out of scope, but worth noting for later

- Configurable hotkey via tray menu (currently hardcoded in config.json).
- Audio feedback chime on record start/stop.
- Visible recording indicator overlay.
- Multiple whisper endpoints with fallback.
- Auto-update.
