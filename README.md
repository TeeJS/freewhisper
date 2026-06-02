# freewhisper

A push-to-talk dictation tool for Windows. Hold a global hotkey, speak, release — and the transcribed text is pasted into whatever window has focus. Audio is captured locally and transcribed by a self-hosted [faster-whisper](https://github.com/SYSTRAN/faster-whisper) server speaking the [Wyoming protocol](https://github.com/rhasspy/wyoming) on your LAN. A Wispr Flow alternative with no cloud, no subscription, and no Python on the client.

**Status:** Working. Single self-contained `.exe`, no installer or runtime dependencies.

## How it works

```
hold hotkey ─▶ capture mic (WASAPI) ─▶ VAD splits speech into chunks at pauses
                                              │
                                  each chunk ─┼─▶ Wyoming whisper server ─▶ text
                                              │
release hotkey ─▶ final chunk ────────────────┘
                                              │
                          text pasted in order ─▶ clipboard + Ctrl+V into focused window
```

Transcription streams: chunks are sent to the server as you pause speaking, so text begins appearing before you release the key. A paste queue reassembles the chunks in order even though they may transcribe out of order.

## Requirements

- Windows 10/11 (x64).
- A reachable Wyoming-protocol faster-whisper server on your network. The standard port is `10300`. (This is the same server type used by Home Assistant's voice pipeline / Rhasspy.)

## Build

```powershell
go build -ldflags="-H windowsgui -s -w" -o freewhisper.exe
```

- `-H windowsgui` suppresses the console window (this is a tray app).
- `-s -w` strips debug symbols to shrink the binary (~8 MB).

The Win32 manifest (modern Common Controls, required by the settings dialog) is embedded automatically via `manifest.syso`. See [`CLAUDE.md`](./CLAUDE.md) if you need to regenerate it.

## First run

Run `freewhisper.exe`. It lands in the system tray and, on first launch, opens the settings window and writes a default `config.json` next to the executable. Fill in your whisper server's **host** and **port**, then click **Save**.

## Configuration

Settings live in `config.json` beside the `.exe`. Edit them via the tray menu (**right-click → Settings…**) or by hand. Fields:

| Key | Meaning | Default |
| --- | --- | --- |
| `whisper_host` | IP or DNS name of the Wyoming server | _(empty — must set)_ |
| `whisper_port` | TCP port of the server | `10300` |
| `language` | Language hint, e.g. `"en"`; empty = auto-detect | `"en"` |
| `hotkey_modifiers` | Any of `"Ctrl"`, `"Alt"`, `"Shift"`, `"Win"` (at least one) | `["Ctrl"]` |
| `hotkey_key` | The non-modifier key, e.g. `"Backtick"`, `"Space"`, `"A"`, `"F8"` | `"Backtick"` |
| `notify_color_change` | Swap the tray icon to red while recording | `false` |
| `notify_beep` | Short chime on record start/stop | `false` |
| `silence_duration_ms` | Pause length that ends a chunk; shorter = more responsive, less accurate | `400` |

Server, language, notification, and silence settings apply immediately on Save. **Changing the hotkey requires restarting the app** (the chord is registered with Windows once at startup).

## Usage

Hold the hotkey (default **Ctrl+`**), speak, release. The text appears at your cursor.

The tool stages text on the clipboard and sends Ctrl+V; it restores your previous clipboard **text** afterward. Non-text clipboard contents (images, files) are not preserved — a documented limitation.

## Debugging

Logs are written to `%LOCALAPPDATA%\freewhisper\debug.log`.

To capture the raw microphone audio for troubleshooting, set the `FREEWHISPER_DEBUG_WAV` environment variable (to any value) before launching — each recording is then also written to `%LOCALAPPDATA%\freewhisper\test.wav`. Leave it unset for normal use; you don't want every dictation saved to disk.

## License

Personal project. Not yet licensed.
