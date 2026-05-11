// Package main is the freewhisper entry point.
//
// Milestone 5 (MVP-complete) scope: while Ctrl+` is held, capture mic audio
// (recorder.go); on release, send the PCM to the Wyoming whisper server
// (transcriber.go), then paste the resulting text into the active window
// (paster.go). The captured audio is still also written to test.wav for
// sanity-checking on next-time-at-keyboard debug sessions.
//
// Endpoint and language come from config.json (loaded at startup, falls
// back to compiled defaults if missing).
//
// We log to a file (not stdout) because the app is built with `-H windowsgui`,
// which suppresses the console window. Anything written to stdout/stderr in
// such a build is silently dropped, so file logging is the only way to see
// what the program is doing.
package main

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"
	"golang.design/x/hotkey"
)

// Two tray-icon variants embedded at compile time. The idle icon (blue F)
// shows when the app is sitting waiting for a hotkey; the recording icon
// (red F) replaces it while the user is holding the chord, but only when
// the NotifyColorChange setting is on. We swap by calling systray.SetIcon
// with the appropriate byte slice at runtime.

//go:embed icon.ico
var iconIdle []byte

//go:embed icon_recording.ico
var iconRecording []byte

// appConfig holds the runtime configuration. Loaded once at startup and
// then read-only — we treat config as immutable for the process lifetime.
// (Hot-reload is a feature we'd add later, after there's any need for it.)
var appConfig Config

// configExisted tracks whether config.json was present at startup. The
// settings GUI uses this to auto-open on first run (when false).
var configExisted bool

func main() {
	setupLogging()
	log.Print("freewhisper starting")
	cfgPath := ""
	appConfig, cfgPath, configExisted = LoadConfig()
	if !configExisted {
		// First run: write the defaults to disk so the user has a file
		// to inspect/edit and to anchor the settings GUI's auto-open
		// behavior. Failure to write is logged but non-fatal.
		if err := appConfig.Save(); err != nil {
			log.Printf("config: first-run save failed: %v", err)
		} else {
			log.Printf("config: wrote default config to %s (first run)", cfgPath)
		}
	}
	log.Printf("config: endpoint=%s language=%s hotkey=%s silenceMs=%d (from %s)",
		appConfig.Endpoint(),
		appConfig.Language,
		HotkeyDisplay(appConfig.HotkeyModifiers, appConfig.HotkeyKey),
		appConfig.SilenceDurationMs,
		cfgPath,
	)
	systray.Run(onReady, onExit)
}

// setupLogging redirects Go's default logger to %LOCALAPPDATA%\freewhisper\debug.log.
//
// %LOCALAPPDATA% (typically C:\Users\<you>\AppData\Local) is the standard
// Windows location for per-user, non-roaming application data. Unlike
// %APPDATA%, it doesn't sync to other machines or to OneDrive, so log files
// stay local and don't bloat your cloud storage.
//
// If anything goes wrong here (env var missing, disk full, permissions), we
// silently fall back to the default logger destination (stderr, which in a
// `-H windowsgui` build goes nowhere). That's a known limitation of GUI-only
// Windows binaries — there's no good place to surface bootstrap errors.
// We'll address it later if it ever bites us; for now, "no logs" is an
// acceptable failure mode for milestone 2.
func setupLogging() {
	appData := os.Getenv("LOCALAPPDATA")
	if appData == "" {
		return
	}
	dir := filepath.Join(appData, "freewhisper")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	logPath := filepath.Join(dir, "debug.log")
	// os.O_APPEND so each run adds to the existing file instead of overwriting.
	// 0644 = rw for owner, r for group/other (Windows largely ignores this but
	// it's good Unix hygiene and Go's os package wants the mode argument).
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	log.SetOutput(f)
	// LstdFlags = date + seconds. Lmicroseconds gives us sub-second precision,
	// useful when reasoning about hotkey timing later.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

func onReady() {
	systray.SetIcon(iconIdle)
	systray.SetTitle("FreeWhisper")
	systray.SetTooltip("FreeWhisper (idle)")

	// Menu items, in display order: Settings, then a separator, then Quit.
	// AddMenuItemCheckbox / separator aren't needed yet — keep it minimal.
	mSettings := systray.AddMenuItem("Settings…", "Open settings window")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Exit FreeWhisper")

	go func() {
		for range mSettings.ClickedCh {
			OpenSettings()
		}
	}()
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	// registerHotkey is in its own function to keep onReady readable.
	// We spawn the event loop as a goroutine so it runs concurrently with
	// the systray menu without blocking either side.
	go registerHotkey()

	// First-run UX: if config.json didn't exist at startup, the user
	// almost certainly hasn't set the whisper endpoint yet. Pop the
	// settings window automatically so they don't have to discover the
	// tray menu first.
	if !configExisted {
		OpenSettings()
	}
}

// applyConfigChange swaps the running config to next. Server/language/
// notification settings take effect immediately because all the code
// paths that consume them read appConfig fresh on each operation
// (transcribe, indicator). The hotkey, by contrast, was registered with
// Win32 once at startup and the press loop is blocked on the old object's
// channels — changing the chord requires a restart. The settings GUI
// surfaces this caveat in its layout, so we don't duplicate the warning
// here.
func applyConfigChange(next Config) {
	oldHotkey := HotkeyDisplay(appConfig.HotkeyModifiers, appConfig.HotkeyKey)
	newHotkey := HotkeyDisplay(next.HotkeyModifiers, next.HotkeyKey)
	appConfig = next
	log.Printf("config updated: endpoint=%s language=%s hotkey=%s silenceMs=%d notify(color=%v beep=%v)",
		next.Endpoint(), next.Language, newHotkey, next.SilenceDurationMs,
		next.NotifyColorChange, next.NotifyBeep)
	if oldHotkey != newHotkey {
		log.Printf("hotkey changed (%s → %s); restart required to apply", oldHotkey, newHotkey)
	}
}

func onExit() {
	log.Print("freewhisper exiting")
}

// registerHotkey registers the user-configured push-to-talk chord as a
// global hotkey and reports debounced press/release events to the log.
//
// Modifier/key values come from appConfig (see config.go) and get parsed
// via the lookup tables in hotkeymap.go. Default chord is Ctrl+`
// (Ctrl+backtick): one-handed reachable, top-left of the keyboard, no
// collision with Excel's Ctrl+Space (column-select), VS Code's Ctrl+Space
// (autocomplete), or Alt+Space (window system menu).
//
// Why a debounce window: under the hood, golang.design/x/hotkey calls Win32's
// RegisterHotKey, which causes WM_HOTKEY messages to be re-posted at the
// keyboard auto-repeat rate (~10ms apart on default settings) while the chord
// is held. The library detects "release" by polling GetAsyncKeyState after
// each WM_HOTKEY, which produces a spurious UP→DOWN pair on every repeat
// cycle. Without debouncing, holding the chord for 2 seconds produces ~200
// fake press cycles instead of one continuous press — disastrous for
// push-to-talk semantics.
//
// The debounce: only the first DOWN of a press burst and the last UP of that
// burst are reported. We do this by waiting `debounceWindow` after each UP
// to see if another DOWN follows. If one does, it was auto-repeat noise; if
// it doesn't, the release is real.
func registerHotkey() {
	// 80ms comfortably exceeds the keyboard auto-repeat interval (default
	// Windows repeat rate is ~30/sec = 33ms; max-fast is around 30ms). If a
	// user is genuinely tapping the chord faster than 12 times per second to
	// trigger separate press cycles, they will be merged — acceptable trade.
	const debounceWindow = 80 * time.Millisecond

	mods, err := ParseModifiers(appConfig.HotkeyModifiers)
	if err != nil {
		log.Printf("hotkey register: %v", err)
		return
	}
	if len(mods) == 0 {
		log.Print("hotkey register: at least one modifier (Ctrl/Alt/Shift/Win) required; check config.json")
		return
	}
	key, err := ParseKey(appConfig.HotkeyKey)
	if err != nil {
		log.Printf("hotkey register: %v", err)
		return
	}
	display := HotkeyDisplay(appConfig.HotkeyModifiers, appConfig.HotkeyKey)

	hk := hotkey.New(mods, key)
	if err := hk.Register(); err != nil {
		log.Printf("hotkey register failed (%s): %v", display, err)
		return
	}
	log.Printf("hotkey registered: %s", display)

	for {
		// Wait for the first DOWN that starts a press cycle, then start
		// the chunked recorder + the consumer goroutine that ships chunks
		// to whisper as VAD reports them.
		<-hk.Keydown()
		log.Print("hotkey DOWN — streaming started")
		startRecordingIndicator()
		rec := StartChunkedRecording(appConfig.SilenceDurationMs)
		consumerDone := startStreamingConsumer(rec)

		// Drain UP/DOWN pairs caused by auto-repeat until we see an UP
		// that *isn't* followed by another DOWN within debounceWindow.
		// That UP is the genuine release.
	drain:
		for {
			<-hk.Keyup()
			timer := time.NewTimer(debounceWindow)
			select {
			case <-hk.Keydown():
				// Auto-repeat blip — key is still held. Discard both
				// events and keep waiting for the real release.
				if !timer.Stop() {
					<-timer.C
				}
			case <-timer.C:
				// Genuine release: stop the recorder. The consumer
				// goroutine will drain any final chunk and the paste
				// queue will flush in order. We wait on consumerDone so
				// the next press doesn't race with in-flight pastes.
				pcm, err := rec.Stop()
				stopRecordingIndicator()
				if err != nil {
					log.Printf("recording failed: %v", err)
				}
				<-consumerDone
				saveCapturedWAV(pcm)
				break drain
			}
		}
	}
}

// saveCapturedWAV writes the captured PCM bytes to test.wav in
// %LOCALAPPDATA%\freewhisper\, alongside debug.log. We log the byte count,
// the implied duration, and the full path so the user can find and play
// back the file to sanity-check the capture.
func saveCapturedWAV(pcm []byte) {
	appData := os.Getenv("LOCALAPPDATA")
	if appData == "" {
		log.Printf("LOCALAPPDATA not set; can't save WAV")
		return
	}
	path := filepath.Join(appData, "freewhisper", "test.wav")
	if err := writeWAV(path, pcm, captureSampleRate, captureChannels, captureBitsPerSample); err != nil {
		log.Printf("WAV write failed: %v", err)
		return
	}
	// Duration = bytes / (sampleRate * blockAlign).
	// blockAlign = channels * bitsPerSample/8 = 1 * 2 = 2.
	durSec := float64(len(pcm)) / float64(captureSampleRate*uint32(captureChannels*captureBitsPerSample/8))
	log.Printf("hotkey UP — recorded %d bytes (%.2fs) → %s", len(pcm), durSec, path)
}

// startStreamingConsumer wires a ChunkedRecorder's output up to whisper
// and the paste queue. Returns a channel that closes once every chunk
// has been transcribed AND pasted (or skipped on error). The hotkey loop
// blocks on this channel after rec.Stop() so the next press can't race
// with an in-flight paste.
//
// Concurrency model:
//
//   recorder ───chunks───▶ this goroutine
//                              │ spawns one transcribe goroutine per chunk
//                              ▼
//                          transcribe (parallel)
//                              │ submits result with seq number
//                              ▼
//                          orderedPaster (single goroutine, in-order pastes)
//
// Transcribes run in parallel because whisper is the slow step and we
// want to pipeline as much as possible. The orderedPaster reassembles
// in seq order so chunk 2's text never lands before chunk 1's, even if
// chunk 2 finished transcribing first.
func startStreamingConsumer(rec *ChunkedRecorder) <-chan struct{} {
	done := make(chan struct{})

	if !appConfig.EndpointConfigured() {
		// Drain chunks but don't try to transcribe — log once and bail.
		go func() {
			defer close(done)
			logged := false
			for range rec.Chunks() {
				if !logged {
					log.Print("transcribe skipped: whisper_host/whisper_port not set (right-click tray → Settings)")
					logged = true
				}
			}
		}()
		return done
	}

	go func() {
		defer close(done)
		paster := newOrderedPaster()
		var wg sync.WaitGroup

		for chunk := range rec.Chunks() {
			chunk := chunk // capture for goroutine
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				text, err := Transcribe(appConfig.Endpoint(), appConfig.Language, chunk.PCM)
				elapsed := time.Since(start)
				if err != nil {
					log.Printf("chunk %d transcribe failed (%.2fs): %v", chunk.Seq, elapsed.Seconds(), err)
				} else {
					log.Printf("chunk %d transcript (%.2fs, %d bytes audio): %q",
						chunk.Seq, elapsed.Seconds(), len(chunk.PCM), text)
				}
				paster.Submit(chunk.Seq, text, err)
			}()
		}
		// All chunks received from the recorder — wait for outstanding
		// transcribes to deposit results, then close the paster so it
		// flushes whatever's left in order.
		wg.Wait()
		paster.Close()
	}()

	return done
}

// orderedPaster receives transcription results out-of-order and pastes
// them into the focused window in seq order. It owns a single goroutine
// (started by newOrderedPaster), serializes pastes through a channel,
// and exits cleanly when Close() is called and all submitted work has
// been processed.
type orderedPaster struct {
	incoming chan pasteResult
	done     chan struct{}
}

type pasteResult struct {
	seq  int
	text string
	err  error
}

func newOrderedPaster() *orderedPaster {
	p := &orderedPaster{
		incoming: make(chan pasteResult, 32),
		done:     make(chan struct{}),
	}
	go p.run()
	return p
}

// Submit hands a transcription result to the paster. Safe to call from
// any goroutine. Non-blocking unless the incoming buffer (32 deep) is
// completely full, which would require 32+ chunks pending paste — very
// unlikely in practice.
func (p *orderedPaster) Submit(seq int, text string, err error) {
	p.incoming <- pasteResult{seq: seq, text: text, err: err}
}

// Close signals no more submissions are coming and blocks until the
// paster has drained its queue and pasted everything that can be pasted.
// Out-of-order tail items (e.g. seq 5 arrived but seq 4 never did) are
// dropped on close — better than waiting forever for a transcribe that
// errored mid-flight.
func (p *orderedPaster) Close() {
	close(p.incoming)
	<-p.done
}

func (p *orderedPaster) run() {
	defer close(p.done)

	pending := map[int]pasteResult{}
	nextSeq := 0

	flush := func() {
		// Paste anything that's now in order.
		for {
			r, ok := pending[nextSeq]
			if !ok {
				return
			}
			delete(pending, nextSeq)
			p.pasteOne(r, nextSeq)
			nextSeq++
		}
	}

	for r := range p.incoming {
		pending[r.seq] = r
		flush()
	}

	// Channel closed. Anything still in pending is out-of-order tail —
	// either a transcribe errored before reporting (and we never got
	// that seq) or sequencing got confused. Either way, drop these
	// silently; the alternative (blocking forever) is worse.
}

// pasteOne handles the per-chunk paste logic: trim whitespace, add a
// leading space if this isn't the first chunk in the session (so words
// flow together), call Paste(). Errors are logged but don't stop the
// queue.
func (p *orderedPaster) pasteOne(r pasteResult, seq int) {
	if r.err != nil {
		return // already logged at transcribe site
	}
	text := strings.TrimSpace(r.text)
	if text == "" {
		return
	}
	// Chunks after the first get a leading space so consecutive chunks
	// read as a sentence rather than glued-together words.
	if seq > 0 {
		text = " " + text
	}
	if err := Paste(text); err != nil {
		log.Printf("paste seq %d failed: %v", seq, err)
	}
}
