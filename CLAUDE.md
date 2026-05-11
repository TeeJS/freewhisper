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
[hotkey released]
       ↓
[wrap PCM in WAV header]
       ↓
[HTTP multipart POST to whisper endpoint]
       ↓
[parse JSON, extract text]
       ↓
[clipboard SetText + SendInput Ctrl+V into active window]
       ↓
[idle, wait for next hotkey press]
```

## File layout

```
freewhisper/
├── CLAUDE.md           ← this file
├── README.md           ← user-facing setup/usage docs
├── go.mod
├── go.sum
├── main.go             ← entry point, tray icon, hotkey wiring, orchestration
├── recorder.go         ← WASAPI mic capture → []byte WAV
├── transcriber.go      ← HTTP POST to whisper, JSON parsing
├── paster.go           ← clipboard write + SendInput Ctrl+V
├── config.go           ← config struct, load from config.json
├── config.example.json ← committed example config (no secrets)
├── icon.ico            ← tray icon
└── build.ps1           ← build script (go build with appropriate flags)
```

`config.json` is gitignored. `config.example.json` is committed.

## Key dependencies

- `github.com/getlantern/systray` — tray icon
- `golang.design/x/hotkey` — global hotkey registration
- `github.com/go-ole/go-ole` + `github.com/moutend/go-wca` — WASAPI mic capture (COM wrappers)
- `golang.org/x/sys/windows` — official Windows syscall bindings, used for SendInput and clipboard

All MIT/BSD. No exotic transitive dependencies expected.

## Whisper endpoint

The home faster-whisper server. **TBD — exact URL, port, and API shape need to be confirmed before transcriber.go is written.** Assumption is an OpenAI-compatible `POST /v1/audio/transcriptions` endpoint accepting multipart form with a `file` field and returning `{"text": "..."}` JSON. If the actual server uses a different API, transcriber.go adapts to match.

User will fill `config.json` with the endpoint URL after first build.

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
