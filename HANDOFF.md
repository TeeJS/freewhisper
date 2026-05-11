# FreeWhisper — Handoff to Claude Code

This is a one-time onboarding doc for the first Claude Code session. After the first commit, this can be archived or deleted — `CLAUDE.md` covers ongoing context. Read both before starting.

## How we got here

This project was scoped out in a conversation with Claude.ai (web). The user wanted a Wispr Flow alternative that uses a self-hosted faster-whisper instance. The conversation walked through several decisions; this section captures them so you don't relitigate.

### Decisions already made (do not relitigate)

1. **Wispr Flow alternative, push-to-talk, hotkey-driven.** Not a continuous dictation tool, not a meeting transcriber. Hold key, speak, release, text appears.

2. **Remote whisper, not local.** The laptop is a thin client. Transcription runs on the user's home GPU box (Unraid Lenovo at `192.168.1.25`, RTX 3060). The laptop only captures audio and pastes text.

3. **Home network only.** The user works from home 95%+ of the time. No need for public DNS, NPMplus reverse proxy, bearer tokens, or TLS. Plain HTTP to an RFC1918 address. If this changes later, auth gets added then.

4. **Compiled language, not Python.** The work laptop has no Python. PyInstaller was considered and rejected — its `--onefile` extraction pattern looks like a packer to behavioral EDR (FortiEDR specifically), and PyInstaller-built binaries have a poor reputation across AV engines.

5. **Go, not C or Rust.** C is the cleanest binary but a steep learning curve for the user. Rust was an option. Go won on: small toolchain footprint (~700MB total), clean static binaries (~8-12MB), familiar syntax for someone with limited programming background, existing minor familiarity (the user has a Go kanban project — TeeJS/golang-kanban).

6. **Ask-forgiveness, not permission, regarding FortiEDR.** The user is aware FortiEDR may flag the app. They've decided to build it and stop using it if it trips an alert. **Do not advise getting an exception added** — that conversation already happened. The user explicitly chose this path.

7. **Project name: `freewhisper`.** Repo to be created on GitHub. Not yet created at handoff time.

### Open questions to resolve early

- **Whisper endpoint specifics.** The user has a running faster-whisper instance (it's the same one feeding the ESP32-P4 / NanoClaw voice pipeline, on the RTX 3060). At handoff time, the exact container image, port, and API shape are unconfirmed. The assumption is OpenAI-compatible `POST /v1/audio/transcriptions` returning `{"text": "..."}`. **Confirm this before writing `transcriber.go`.** A `docker ps` on `192.168.1.25` will reveal the container.

- **Hotkey choice.** Default suggested is something like `Ctrl+Win+Space` or `Right Alt` (push-to-talk feel). User has not picked. Ask before hardcoding.

- **Paste vs type.** Clipboard-then-Ctrl+V is faster and looks less like a keylogger to EDR (no per-character SendInput), but stomps the user's clipboard. Plan: save clipboard contents, write transcription, paste, restore original clipboard. Confirm this approach is acceptable.

## User context (read this carefully)

The user is an ERP Manager at a manufacturing company. Deep infrastructure experience — Windows domain, Docker, FortiSIEM, an extensive home lab, self-hosted AI stack — but **new to programming languages, git workflow, structure, and theory**. They're eager to learn alongside building.

This shapes how to work with them:

- **Explain *why*, not just *what*.** When picking between two patterns, say what you ruled out and why. They're learning the craft, not just shipping the app.
- **One new concept at a time.** Don't introduce goroutines, channels, COM, and Windows messaging in the same commit. Layer it.
- **Don't pattern-match to "experienced dev."** They've moved infrastructure mountains, but `:=` vs `=` and pointer receivers are genuinely new.
- **Comment generously.** Especially around Windows API calls. Treat the codebase as a reference they'll re-read.
- **They push back when answers don't fit.** When that happens, take it at face value — do not defend the previous suggestion. Stop and rework from their constraints.
- **Three failed attempts = stop and research.** Don't keep guessing. Read docs or GitHub issues before proposing the next step.
- **Backup discipline.** Always create timestamped backups before modifying files, unless git is providing that safety. Commit early and often.

## Recommended first session

Don't try to write the whole app in one go. Aim for these milestones in order:

1. **`git init`, push to GitHub.** User will create the repo at `github.com/<their-handle>/freewhisper`. Get an initial commit with `CLAUDE.md`, `HANDOFF.md`, `.gitignore`, and `README.md` (stub). Confirm Go is installed (`go version`). Confirm git is configured.

2. **`go mod init github.com/<user>/freewhisper`.** Verify module setup works. Commit.

3. **Hello-world tray icon.** A `main.go` that uses `getlantern/systray` to put an icon in the tray with one menu item: "Quit." Builds cleanly, runs, quits cleanly. This proves the entire toolchain works end to end. Commit.

4. **Add a global hotkey.** Press the hotkey, log a line to a debug file (no console window in a `-H windowsgui` build). Release the hotkey, log another line. This proves hotkey capture works. Commit.

5. **Add WASAPI mic capture.** Press hotkey → capture audio to a buffer → release hotkey → write the buffer to disk as `test.wav`. Open `test.wav` in any audio player to confirm it sounds right. **This is the hardest part of the project**. Take it slow. Commit when it works.

6. **Add HTTP POST to whisper.** Replace "save to disk" with "POST to endpoint, print response." Confirms the server understands the request and we can parse the response. Commit.

7. **Add paste.** Tie it all together. Commit. **Project is now MVP-complete.**

Each milestone should be a separate commit (or a small set of commits). The user is learning git too — keep the history clean and meaningful.

## Things to avoid

- **Don't write a "complete" first draft.** Build incrementally so each step is comprehensible and testable.
- **Don't suggest alternative architectures partway through.** The architecture is settled (see `CLAUDE.md`). If something genuinely needs to change, raise it explicitly with the user before acting.
- **Don't add features that weren't asked for.** No "while we're here, let me add a config GUI." Stay scoped.
- **Don't modify files outside the repo** without asking.
- **Don't use `configuration.yaml`-style sprawl** — there's no Home Assistant here, but the principle is the same. One config file. One source of truth.

## Reference repos that solve similar problems

If you need to see how another project handled a particular Win32 API call:

- `github.com/Danaor/WhisperType` — Python, but the high-level structure and Win32 sequencing is instructive.
- `golang.design/x/hotkey` — the canonical Go global-hotkey package, has Windows examples.
- `github.com/moutend/go-wca` — WASAPI from Go, has a `examples/` directory with mic capture code.
- `github.com/getlantern/systray` — has clean Windows examples.

Do not copy code verbatim from these without attribution. Read them to understand the pattern, then write the user's version from scratch.

## When in doubt

Re-read `CLAUDE.md`. If it doesn't answer the question, ask the user before guessing.
