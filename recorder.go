// recorder.go: WASAPI microphone capture and WAV file writing.
//
// This file is dense with Windows-specific concepts. Quick glossary you'll
// see used below:
//
//   COM           - Component Object Model. Windows' object-IPC system. Every
//                   Win32 audio call goes through it. Each thread that wants
//                   to call COM functions must first "enter an apartment" via
//                   CoInitializeEx; that's what runtime.LockOSThread is for.
//
//   WASAPI        - Windows Audio Session API. The modern (Vista+) audio API.
//                   Faster, lower-latency, and less buggy than its predecessors
//                   (MME, DirectSound, WaveOut).
//
//   IMMDevice     - "MultiMedia Device". Represents a physical audio device
//                   (a mic, speakers, headphones). We get one by asking
//                   IMMDeviceEnumerator for the default eCapture device.
//
//   IAudioClient  - The stream object. We Initialize() it with a format we
//                   want, Start() it to begin capture, and Stop() it to end.
//
//   IAudioCaptureClient - The buffer pump. After we Start the audio client,
//                   we ask this object "any new packets?", and it hands us
//                   pointers to chunks of recorded PCM data. We copy what we
//                   want, then ReleaseBuffer to tell Windows we're done.
//
// Strategy: we ask WASAPI to deliver audio in our preferred format
// (48 kHz mono 16-bit PCM, which is small, well-supported, and close enough
// to whisper's preferred 16 kHz that the server-side resample is cheap), and
// use the AUTOCONVERTPCM flag so Windows handles any mismatch with the
// device's native format internally.

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
// When this flag is set on a captured packet, the data pointer may be
// invalid garbage — we must write silence (zeros) instead of reading it.
// See https://learn.microsoft.com/en-us/windows/win32/api/audioclient/ne-audioclient-_audclnt_bufferflags
const audClntBufferFlagsSilent uint32 = 0x2

// Recorder is a single push-to-talk capture session. Create one via
// StartRecording(), let it run on its own goroutine, then call Stop() to
// retrieve the captured audio.
type Recorder struct {
	stopCh   chan struct{}     // closed by Stop() to signal the worker to wrap up
	resultCh chan recordResult // worker pushes the final result here exactly once
}

type recordResult struct {
	pcm []byte // raw PCM bytes in the format defined by capture* constants above
	err error
}

// StartRecording begins capturing from the default audio capture device on a
// dedicated goroutine and returns a handle the caller can use to stop and
// retrieve the recording. The returned Recorder is already running by the
// time this function returns.
func StartRecording() *Recorder {
	r := &Recorder{
		stopCh:   make(chan struct{}),
		resultCh: make(chan recordResult, 1),
	}
	go r.run()
	return r
}

// Stop signals the recorder to finish, then blocks until it returns the
// captured PCM bytes (or an error). It is safe to call Stop exactly once
// per Recorder; calling it twice will deadlock.
func (r *Recorder) Stop() ([]byte, error) {
	close(r.stopCh)
	res := <-r.resultCh
	return res.pcm, res.err
}

// run is the worker goroutine entry point. It locks itself to a single OS
// thread for the duration of the capture so all COM calls stay within the
// same apartment — COM is strict about this; cross-thread calls without
// marshalling will silently corrupt state.
func (r *Recorder) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	pcm, err := captureLoop(r.stopCh)
	r.resultCh <- recordResult{pcm: pcm, err: err}
}

// captureLoop is the meat of the recorder: initialize COM, walk the WASAPI
// object graph (enumerator → device → audio client → capture client), spin
// in a poll loop appending packets to a buffer, and tear everything down
// cleanly when stopCh closes.
//
// Returns the accumulated PCM bytes (which may be partial if an error
// occurred mid-capture) and the first error encountered (nil on success).
func captureLoop(stopCh <-chan struct{}) ([]byte, error) {
	// 1. Enter a COM apartment. APARTMENTTHREADED is the right choice for a
	//    GUI-ish app and is what most WASAPI examples use. If COM was already
	//    initialized on this thread (it shouldn't be, but just in case), we
	//    get back an S_FALSE-style error, which we treat as non-fatal.
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if oerr, ok := err.(*ole.OleError); !ok || oerr.Code() != 1 /* S_FALSE */ {
			return nil, fmt.Errorf("CoInitializeEx: %w", err)
		}
	}
	defer ole.CoUninitialize()

	// 2. Create the device enumerator. This is the entry point into the
	//    MMDevice API — from it we can query for the default mic.
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

	// 3. Get the default capture device. The two integer arguments are:
	//      eDataFlow = 1 (eCapture)   — microphones and other inputs
	//      eRole     = 0 (eConsole)   — the device Windows uses for general I/O
	//    These are MMDevice API enum values; go-wca doesn't expose them as
	//    named constants, so we pass the raw integers with a comment.
	var device *wca.IMMDevice
	if err := deviceEnumerator.GetDefaultAudioEndpoint(1 /*eCapture*/, 0 /*eConsole*/, &device); err != nil {
		return nil, fmt.Errorf("GetDefaultAudioEndpoint(eCapture, eConsole): %w", err)
	}
	defer device.Release()

	// 4. Activate IAudioClient on the device. "Activate" is COM-speak for
	//    "give me an interface pointer to this object so I can call methods
	//    on it." It does not start the audio stream — that's Step 7.
	var audioClient *wca.IAudioClient
	if err := device.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &audioClient); err != nil {
		return nil, fmt.Errorf("device.Activate(IAudioClient): %w", err)
	}
	defer audioClient.Release()

	// 5. Build the format we want WASAPI to deliver: 48 kHz mono 16-bit PCM.
	//    NBlockAlign = bytes per "frame" (one sample across all channels)
	//                = NChannels * WBitsPerSample / 8
	//    NAvgBytesPerSec = NSamplesPerSec * NBlockAlign
	//    These are derived but must be filled in manually — WASAPI uses them
	//    for buffer sizing internally.
	blockAlign := captureChannels * captureBitsPerSample / 8
	wfx := &wca.WAVEFORMATEX{
		WFormatTag:      wca.WAVE_FORMAT_PCM,
		NChannels:       captureChannels,
		NSamplesPerSec:  captureSampleRate,
		NAvgBytesPerSec: captureSampleRate * uint32(blockAlign),
		NBlockAlign:     blockAlign,
		WBitsPerSample:  captureBitsPerSample,
		// CbSize = 0 for plain PCM (no extra format-specific data follows)
	}

	// 6. Initialize the audio stream in shared mode with format conversion
	//    enabled. The two flags together tell the audio engine:
	//      AUTOCONVERTPCM        — "if my requested format doesn't match the
	//                              device's native format, insert a converter
	//                              for me instead of failing"
	//      SRC_DEFAULT_QUALITY   — "use the medium-quality resampler for that
	//                              conversion" (vs. fast/low-quality)
	//    Buffer duration: 1 second (10,000,000 100-ns units). That's our
	//    safety margin — if our poll loop falls behind by up to 1 second,
	//    no data is lost.
	//
	//    nsPeriodicity must be 0 in shared mode (Windows manages timing).
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

	// 7. Grab the IAudioCaptureClient companion interface that we'll poll
	//    to pull packets out of WASAPI's ring buffer.
	var captureClient *wca.IAudioCaptureClient
	if err := audioClient.GetService(wca.IID_IAudioCaptureClient, &captureClient); err != nil {
		return nil, fmt.Errorf("audioClient.GetService(IAudioCaptureClient): %w", err)
	}
	defer captureClient.Release()

	// 8. Figure out how often to poll. Default device period is the audio
	//    engine's natural cadence (~10 ms typically). We poll twice per
	//    period — fast enough to never miss data, slow enough to not burn
	//    CPU spinning.
	var defaultPeriod, minPeriod wca.REFERENCE_TIME
	_ = audioClient.GetDevicePeriod(&defaultPeriod, &minPeriod)
	pollInterval := time.Duration(defaultPeriod) * 100 // 100-ns units → nanoseconds
	if pollInterval <= 0 || pollInterval > 50*time.Millisecond {
		pollInterval = 10 * time.Millisecond // sane fallback
	}
	pollInterval /= 2

	// 9. Start the stream. From this point until audioClient.Stop(), Windows
	//    is actively writing mic samples into our buffer.
	if err := audioClient.Start(); err != nil {
		return nil, fmt.Errorf("audioClient.Start: %w", err)
	}
	defer audioClient.Stop()

	// 10. Poll loop. Each iteration drains all available packets. We exit
	//     when stopCh closes (caller hit hotkey up) or a drain error occurs.
	var buf bytes.Buffer
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	drain := func() error {
		for {
			var packetFrames uint32
			if err := captureClient.GetNextPacketSize(&packetFrames); err != nil {
				return fmt.Errorf("GetNextPacketSize: %w", err)
			}
			if packetFrames == 0 {
				return nil // no more packets ready right now
			}

			var dataPtr *byte
			var framesRead, packetFlags uint32
			if err := captureClient.GetBuffer(&dataPtr, &framesRead, &packetFlags, nil, nil); err != nil {
				return fmt.Errorf("GetBuffer: %w", err)
			}

			if framesRead > 0 {
				sizeBytes := int(framesRead) * int(wfx.NBlockAlign)
				if packetFlags&audClntBufferFlagsSilent != 0 {
					// Silent packet: data pointer may be junk. Emit zeros.
					buf.Write(make([]byte, sizeBytes))
				} else {
					// unsafe.Slice wraps WASAPI's buffer in a Go slice so we
					// can read it. buf.Write copies the bytes into our own
					// storage, so we're safe to ReleaseBuffer afterward.
					buf.Write(unsafe.Slice(dataPtr, sizeBytes))
				}
			}

			if err := captureClient.ReleaseBuffer(framesRead); err != nil {
				return fmt.Errorf("ReleaseBuffer: %w", err)
			}
		}
	}

	for {
		select {
		case <-stopCh:
			// One final drain in case packets arrived after the last tick.
			if err := drain(); err != nil {
				return buf.Bytes(), err
			}
			return buf.Bytes(), nil
		case <-ticker.C:
			if err := drain(); err != nil {
				return buf.Bytes(), err
			}
		}
	}
}

// writeWAV serializes raw PCM bytes to a standard WAV file at `path`. The
// format constants (sample rate, channels, bits) must match the data — we
// don't inspect the bytes, we just describe them in the header.
//
// WAV / RIFF file layout (all little-endian, no padding):
//
//	"RIFF" (4 bytes)
//	file size minus 8 (uint32)
//	"WAVE" (4 bytes)
//	"fmt " (4 bytes, note trailing space)
//	fmt chunk size = 16 for PCM (uint32)
//	format tag = 1 for PCM (uint16)
//	channels (uint16)
//	sample rate (uint32)
//	byte rate = sampleRate * channels * bitsPerSample/8 (uint32)
//	block align = channels * bitsPerSample/8 (uint16)
//	bits per sample (uint16)
//	"data" (4 bytes)
//	data size in bytes (uint32)
//	... PCM samples ...
//
// This is the simplest variant — no LIST/INFO chunks, no extended fmt,
// no padding. Every audio player on the planet will read it.
func writeWAV(path string, pcm []byte, sampleRate uint32, channels uint16, bitsPerSample uint16) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	byteRate := sampleRate * uint32(channels) * uint32(bitsPerSample) / 8
	blockAlign := channels * bitsPerSample / 8
	dataSize := uint32(len(pcm))

	// "file size minus 8" = (everything after the first 8 bytes).
	// Everything before "data" header is 12 + 8 + 16 = 36 bytes, plus the
	// 8-byte "data" + size header which is included in this count.
	// So: 36 (header) + dataSize. The leading "RIFF" + size = 8 bytes
	// are excluded by definition.
	chunkSize := 36 + dataSize

	// Use a single buffered writer? Honestly the header is only ~44 bytes
	// then a single Write of the body, so plain os.File is fine.
	if err := writeHeader(f, chunkSize, sampleRate, channels, bitsPerSample, byteRate, blockAlign, dataSize); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	if _, err := f.Write(pcm); err != nil {
		return fmt.Errorf("write data: %w", err)
	}
	return nil
}

// writeHeader factored out only to keep writeWAV's body readable.
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
	if err := binary.Write(w, le, uint16(1)); err != nil { // WAVE_FORMAT_PCM
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
