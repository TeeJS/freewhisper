# freewhisper

A push-to-talk dictation tool for Windows. Holds a global hotkey, records mic audio, posts it to a self-hosted [faster-whisper](https://github.com/SYSTRAN/faster-whisper) server, and pastes the transcribed text into whatever window has focus.

**Status:** Work in progress. Not yet usable.

## Goals

- Single self-contained `.exe`, no installer, no runtime dependencies, no Python.
- Talks to a remote whisper endpoint on your home LAN.
- Push-to-talk ergonomics: hold key, speak, release, text appears.

See [`CLAUDE.md`](./CLAUDE.md) for design notes and architecture.

## Build (eventually)

```powershell
go build -ldflags="-H windowsgui -s -w" -o freewhisper.exe
```

## License

Personal project. Not yet licensed.
