// recorder.go: WASAPI microphone capture with VAD-driven chunking.
//
// As of Phase 3 the recorder no longer waits for hotkey release before
// surfacing audio. It splits the audio into utterance-sized chunks at
// natural pause boundaries (detected by vad.go) and emits each chunk as
// it happens, so a downstream pipeline can transcribe and paste partial
// results while the user is still speaking.
//
// This file is dense with Windows-specific concepts. Quick glossary:
//
//   COM           - Component Object Model. Windows' object-IPC system. Every
//                   Win32 audio call goes through it. Each thread that wants
//                   to call COM functions must first "enter an apartment" via
//                   CoInitializeEx; that's what runtime.LockOSThread is for.
//
//   WASAPI        - Windows Audio Session API. The modern (Vista+) audio API.
//
//   IMMDevice     - Represents a physical audio device. We get one by asking
//                   IMMDeviceEnumerator for the default eCapture device.
//
//   IAudioClient  - The stream object. Initialize() with a format we want,
//                   Start() to begin capture, Stop() to end.
//
//   IAudioCaptureClient - The buffer pump. We poll GetNextPacketSize and
//                   GetBuffer to pull out chunks of recorded PCM.
//
// Strategy: ask WASAPI to deliver 48 kHz mono 16-bit PCM. With
// AUTOCONVERTPCM, the audio engine resamples from the device's native
// format. We slice each WASAPI packet into 20 ms VAD frames, feed each
// frame to the detector, and watch for "speech then sustained silence"
// transitions — that's a chunk boundary.

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// Audio format constants. These define the format we *ask* WASAPI to deliver.
// With the AUTOCONVERTPCM flag in Initialize, the audio engine will resample
// and downmix from the device's native format (often 48 kHz stereo 32-bit
// float) to these settings transparently.
const (
	captureSampleRate    uint32 = 48000 // 48 kHz — universally supported, easy resample to 16 kHz on the server
	captureChannels      uint16 = 1     // mono — speech doesn't benefit from stereo, and it halves the payload
	captureBitsPerSample uint16 = 16    // 16-bit signed PCM — standard "studio" depth, plenty for voice
)

// AUDCLNT_BUFFERFLAGS_SILENT (defined by WASAPI; not exposed by go-wca).
const audClntBufferFlagsSilent uint32 = 0x2

// Chunk is one VAD-bounded segment of audio to be transcribed. The Seq
// field lets the downstream paste queue restore order if transcribe
// finishes out of order (chunk 2 might come back from whisper faster than
// chunk 1 if the server is doing parallel processing).
type Chunk struct {
	Seq int    // 0-indexed within this recording session
	PCM []byte // 48 kHz mono 16-bit LE PCM bytes for this chunk
}

// ChunkedRecorder runs a single push-to-talk recording session that emits
// VAD-bounded chunks as the user pauses speaking. Create with
// StartChunkedRecording, consume r.Chunks(), then call r.Stop() when the
// hotkey is released — it sends a final chunk (if any audio remains) and
// closes the chunks channel.
type ChunkedRecorder struct {
	silenceFrames int // number of consecutive silence frames that = a boundary

	chunks chan Chunk    // VAD-detected chunk boundaries land here
	stopCh chan struct{} // closed by Stop() to signal the worker
	doneCh chan struct{} // closed by run() after final chunk + cleanup

	// fullPCM and err are set by run() before closing doneCh and read
	// only by Stop() after waiting on doneCh — no concurrent access.
	fullPCM []byte
	err     error
}

// StartChunkedRecording begins capturing and returns a recorder. The
// returned recorder is already running by the time this function returns;
// the caller should consume r.Chunks() in a goroutine, then call r.Stop()
// when ready.
//
// silenceDurationMs controls how long a pause must last (in milliseconds)
// before the recorder cuts a chunk boundary. Comes from config.
func StartChunkedRecording(silenceDurationMs int) *ChunkedRecorder {
	silenceFrames := silenceDurationMs / vadFrameMs
	if silenceFrames < 1 {
		silenceFrames = 1
	}
	r := &ChunkedRecorder{
		silenceFrames: silenceFrames,
		// 16-deep buffer = plenty for typical speech (each chunk is
		// hundreds of ms, downstream paster drains quickly).
		chunks: make(chan Chunk, 16),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
	go r.run()
	return r
}

// Chunks returns the channel on which boundary-detected chunks arrive.
// The channel is closed after Stop() returns (well, after the worker
// finishes final cleanup).
func (r *ChunkedRecorder) Chunks() <-chan Chunk {
	return r.chunks
}

// Stop signals the recorder to wrap up and blocks until the worker has
// emitted any final pending audio as the last chunk and closed the
// chunks channel. Returns the full concatenated PCM (useful for writing
// test.wav for debug) and the first capture error encountered (nil on
// clean shutdown).
func (r *ChunkedRecorder) Stop() ([]byte, error) {
	close(r.stopCh)
	<-r.doneCh
	return r.fullPCM, r.err
}

// run is the worker goroutine entry point. We LockOSThread so all COM
// calls stay within the same apartment. We close r.chunks and r.doneCh
// on exit so consumers can detect the end.
func (r *ChunkedRecorder) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(r.doneCh)
	defer close(r.chunks)

	r.fullPCM, r.err = r.captureLoop()
}

// captureLoop initializes WASAPI, then spins in a poll loop slicing each
// audio packet into 20 ms VAD frames and emitting chunks at boundaries.
// Returns the full concatenated PCM and any error.
func (r *ChunkedRecorder) captureLoop() ([]byte, error) {
	// 1. Enter a COM apartment.
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if oerr, ok := err.(*ole.OleError); !ok || oerr.Code() != 1 {
			return nil, fmt.Errorf("CoInitializeEx: %w", err)
		}
	}
	defer ole.CoUninitialize()

	// 2. Device enumerator.
	var deviceEnumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator,
		0,
		wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator,
		&deviceEnumerator,
	); err != nil {
		return nil, fmt.Errorf("CoCreateInstance(MMDeviceEnumerator): %w", err)
	}
	defer deviceEnumerator.Release()

	// 3. Default capture (eCapture=1) device for console (eConsole=0) role.
	var device *wca.IMMDevice
	if err := deviceEnumerator.GetDefaultAudioEndpoint(1, 0, &device); err != nil {
		return nil, fmt.Errorf("GetDefaultAudioEndpoint(eCapture, eConsole): %w", err)
	}
	defer device.Release()

	// 4. Activate IAudioClient.
	var audioClient *wca.IAudioClient
	if err := device.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &audioClient); err != nil {
		return nil, fmt.Errorf("device.Activate(IAudioClient): %w", err)
	}
	defer audioClient.Release()

	// 5. Requested format: 48 kHz mono 16-bit PCM.
	blockAlign := captureChannels * captureBitsPerSample / 8
	wfx := &wca.WAVEFORMATEX{
		WFormatTag:      wca.WAVE_FORMAT_PCM,
		NChannels:       captureChannels,
		NSamplesPerSec:  captureSampleRate,
		NAvgBytesPerSec: captureSampleRate * uint32(blockAlign),
		NBlockAlign:     blockAlign,
		WBitsPerSample:  captureBitsPerSample,
	}

	// 6. Initialize with AUTOCONVERTPCM + SRC_DEFAULT_QUALITY.
	flags := uint32(wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY)
	const bufferDuration100ns = wca.REFERENCE_TIME(10_000_000) // 1 second
	if err := audioClient.Initialize(
		wca.AUDCLNT_SHAREMODE_SHARED,
		flags,
		bufferDuration100ns,
		0,
		wfx,
		nil,
	); err != nil {
		return nil, fmt.Errorf("audioClient.Initialize: %w", err)
	}

	// 7. IAudioCaptureClient.
	var captureClient *wca.IAudioCaptureClient
	if err := audioClient.GetService(wca.IID_IAudioCaptureClient, &captureClient); err != nil {
		return nil, fmt.Errorf("audioClient.GetService: %w", err)
	}
	defer captureClient.Release()

	// 8. Polling cadence.
	var defaultPeriod, minPeriod wca.REFERENCE_TIME
	_ = audioClient.GetDevicePeriod(&defaultPeriod, &minPeriod)
	pollInterval := time.Duration(defaultPeriod) * 100
	if pollInterval <= 0 || pollInterval > 50*time.Millisecond {
		pollInterval = 10 * time.Millisecond
	}
	pollInterval /= 2

	// 9. Go.
	if err := audioClient.Start(); err != nil {
		return nil, fmt.Errorf("audioClient.Start: %w", err)
	}
	defer audioClient.Stop()

	// 10. Chunking state machine.
	//
	// pending     - raw WASAPI bytes not yet diced into 20 ms frames
	// chunkBuf    - current chunk-in-progress, shipped at next silence
	// fullBuf     - everything (for test.wav)
	// preRoll     - small ring buffer of the most recent N frames of
	//               pre-speech audio. When VAD first detects speech in
	//               a chunk, we prepend the ring's contents to chunkBuf
	//               so we don't miss the first phoneme or two — by the
	//               time the energy detector decides "yes that's
	//               speech," the user has usually already vocalized
	//               20–80 ms of audio we'd otherwise drop on the floor.
	//
	// inSpeech         - have we observed at least one speech frame in
	//                    this chunk? Used to gate silence-counting and
	//                    pre-roll prepending.
	// silenceFramesSeen - consecutive silence frames since last speech.
	//                    Boundary fires when this reaches r.silenceFrames.
	var pending bytes.Buffer
	var chunkBuf bytes.Buffer
	var fullBuf bytes.Buffer
	vad := NewVAD()
	frameBytes := FrameBytes()
	// 12 frames × 20 ms = 240 ms of pre-roll. Enough to cover the VAD
	// calibration window (200 ms) plus typical detector latency without
	// inflating chunks meaningfully.
	const preRollFrames = 12
	preRoll := newFrameRing(preRollFrames, frameBytes)
	var inSpeech bool
	var silenceFramesSeen int
	seq := 0

	emit := func() {
		if chunkBuf.Len() == 0 {
			return
		}
		// Copy the bytes so the next chunkBuf reset doesn't clobber what
		// the consumer is reading. bytes.Buffer.Bytes() returns a slice
		// backed by the buffer's internal storage; we own that until
		// chunkBuf is mutated again.
		out := make([]byte, chunkBuf.Len())
		copy(out, chunkBuf.Bytes())
		r.chunks <- Chunk{Seq: seq, PCM: out}
		seq++
		chunkBuf.Reset()
		inSpeech = false
		silenceFramesSeen = 0
	}

	processFrame := func(frame []byte) {
		// Always append to full buffer for test.wav.
		fullBuf.Write(frame)

		isSpeech := vad.IsSpeech(frame)

		if isSpeech {
			if !inSpeech {
				// First speech frame after silence — prepend the
				// pre-roll ring buffer so we don't miss the leading
				// edge of the utterance (VAD calibration + detector
				// latency eats the first ~200 ms otherwise).
				preRoll.appendTo(&chunkBuf)
			}
			inSpeech = true
			silenceFramesSeen = 0
			chunkBuf.Write(frame)
			return
		}
		if inSpeech {
			// Trailing silence after speech: append to current chunk and
			// see if we've accumulated enough silence to cut.
			chunkBuf.Write(frame)
			silenceFramesSeen++
			if silenceFramesSeen >= r.silenceFrames {
				emit()
			}
			return
		}
		// Pre-speech silence between chunks: stash in the pre-roll ring
		// so we can prepend it when speech finally arrives.
		preRoll.push(frame)
	}

	drain := func() error {
		for {
			var packetFrames uint32
			if err := captureClient.GetNextPacketSize(&packetFrames); err != nil {
				return fmt.Errorf("GetNextPacketSize: %w", err)
			}
			if packetFrames == 0 {
				break
			}
			var dataPtr *byte
			var framesRead, packetFlags uint32
			if err := captureClient.GetBuffer(&dataPtr, &framesRead, &packetFlags, nil, nil); err != nil {
				return fmt.Errorf("GetBuffer: %w", err)
			}
			if framesRead > 0 {
				sizeBytes := int(framesRead) * int(wfx.NBlockAlign)
				if packetFlags&audClntBufferFlagsSilent != 0 {
					pending.Write(make([]byte, sizeBytes))
				} else {
					pending.Write(unsafe.Slice(dataPtr, sizeBytes))
				}
			}
			if err := captureClient.ReleaseBuffer(framesRead); err != nil {
				return fmt.Errorf("ReleaseBuffer: %w", err)
			}
		}
		// Slice 20 ms frames out of pending and feed VAD.
		for pending.Len() >= frameBytes {
			frame := make([]byte, frameBytes)
			_, _ = pending.Read(frame)
			processFrame(frame)
		}
		return nil
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			// Final drain so we don't lose audio captured between the
			// last tick and the stop signal.
			if err := drain(); err != nil {
				return fullBuf.Bytes(), err
			}
			// Anything still in chunkBuf is the user's final phrase —
			// emit it as the last chunk regardless of whether silence
			// was detected (they released the hotkey, that's the
			// definitive boundary).
			if chunkBuf.Len() > 0 {
				out := make([]byte, chunkBuf.Len())
				copy(out, chunkBuf.Bytes())
				r.chunks <- Chunk{Seq: seq, PCM: out}
			}
			return fullBuf.Bytes(), nil

		case <-ticker.C:
			if err := drain(); err != nil {
				return fullBuf.Bytes(), err
			}
		}
	}
}

// writeWAV serializes raw PCM bytes to a standard WAV file at `path`.
// Format args must match the data; we don't inspect the bytes, only
// describe them in the header.
//
// WAV / RIFF file layout (little-endian, no padding):
//
//	"RIFF" (4) + (file size - 8) (uint32) + "WAVE" (4)
//	"fmt " (4) + 16 (uint32) + format tag (uint16) + channels (uint16) +
//	   sample rate (uint32) + byte rate (uint32) + block align (uint16) +
//	   bits per sample (uint16)
//	"data" (4) + data size (uint32) + PCM samples
func writeWAV(path string, pcm []byte, sampleRate uint32, channels uint16, bitsPerSample uint16) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	byteRate := sampleRate * uint32(channels) * uint32(bitsPerSample) / 8
	blockAlign := channels * bitsPerSample / 8
	dataSize := uint32(len(pcm))
	chunkSize := 36 + dataSize

	if err := writeHeader(f, chunkSize, sampleRate, channels, bitsPerSample, byteRate, blockAlign, dataSize); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := f.Write(pcm); err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	return nil
}

// frameRing is a tiny fixed-capacity ring buffer of 20 ms PCM frames,
// used by the chunked recorder to keep recent pre-speech audio on hand.
// When VAD finally fires, we prepend the ring's contents to the current
// chunk so the first phoneme of the utterance survives the VAD's
// calibration + detection latency.
//
// All operations are O(1). The internal frames slice has fixed length =
// capacity; push wraps around via modular indexing on next.
type frameRing struct {
	frames   [][]byte // each entry is a frameBytes-sized slice
	capacity int      // max number of frames retained
	next     int      // index of the next slot to write
	filled   int      // number of slots currently populated (≤ capacity)
}

func newFrameRing(capacity, frameBytes int) *frameRing {
	return &frameRing{
		frames:   make([][]byte, capacity),
		capacity: capacity,
	}
}

// push copies frame into the ring. The frame slice is copied so the
// caller is free to reuse the underlying buffer.
func (r *frameRing) push(frame []byte) {
	cp := make([]byte, len(frame))
	copy(cp, frame)
	r.frames[r.next] = cp
	r.next = (r.next + 1) % r.capacity
	if r.filled < r.capacity {
		r.filled++
	}
}

// appendTo writes the ring's stored frames into dst in chronological
// order (oldest first), then clears the ring. We clear so each chunk's
// pre-roll comes only from audio that was actually pre-speech for THIS
// chunk — not stale frames from a previous utterance.
func (r *frameRing) appendTo(dst *bytes.Buffer) {
	// Oldest frame is at position (next - filled), modulo capacity.
	start := (r.next - r.filled + r.capacity) % r.capacity
	for i := 0; i < r.filled; i++ {
		idx := (start + i) % r.capacity
		dst.Write(r.frames[idx])
		r.frames[idx] = nil
	}
	r.next = 0
	r.filled = 0
}

func writeHeader(w io.Writer, chunkSize, sampleRate uint32, channels, bitsPerSample uint16, byteRate uint32, blockAlign uint16, dataSize uint32) error {
	le := binary.LittleEndian
	if _, err := io.WriteString(w, "RIFF"); err != nil {
		return err
	}
	if err := binary.Write(w, le, chunkSize); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "WAVE"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "fmt "); err != nil {
		return err
	}
	if err := binary.Write(w, le, uint32(16)); err != nil {
		return err
	}
	if err := binary.Write(w, le, uint16(1)); err != nil {
		return err
	}
	if err := binary.Write(w, le, channels); err != nil {
		return err
	}
	if err := binary.Write(w, le, sampleRate); err != nil {
		return err
	}
	if err := binary.Write(w, le, byteRate); err != nil {
		return err
	}
	if err := binary.Write(w, le, blockAlign); err != nil {
		return err
	}
	if err := binary.Write(w, le, bitsPerSample); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "data"); err != nil {
		return err
	}
	if err := binary.Write(w, le, dataSize); err != nil {
		return err
	}
	return nil
}
