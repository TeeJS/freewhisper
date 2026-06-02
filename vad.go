// vad.go: a small voice activity detector that classifies 20ms PCM
// frames as "speech" or "silence" based on RMS amplitude.
//
// Why energy-based (RMS) instead of WebRTC VAD or a neural detector:
//
//   * Pure Go, no CGO. WebRTC's reference VAD is C; the Go wrappers
//     for it all use CGO, which would break our "single self-contained
//     .exe, no runtime dependencies" goal — CGO builds need a C
//     toolchain on the build machine and embed a libc dependency at
//     runtime.
//
//   * Good enough for push-to-talk. We're not trying to do open-
//     microphone always-listening — the user only feeds us audio while
//     they're holding the hotkey, so we can assume speech is the
//     dominant signal. The job here is "find the pauses between
//     phrases," not "decide if a random sound is human speech."
//
// Calibration: the first short window of capture sets the noise floor.
// Threshold = noise floor × multiplier, with a sanity minimum so a
// completely silent room doesn't give us a threshold of zero.
//
// All math runs on 16-bit signed little-endian PCM samples (the format
// produced by recorder.go).

package main

import (
	"encoding/binary"
	"math"
)

// VAD frame size in ms. 20 ms is the WebRTC VAD canonical frame size;
// keeping the same shape makes it easy to swap in WebRTC later if we
// want. At 48 kHz mono 16-bit, 20 ms = 1920 bytes per frame.
const vadFrameMs = 20

// FrameBytes returns the byte-size of one VAD frame at the capture format.
// Centralized so the recorder can slice its WASAPI packets correctly.
func FrameBytes() int {
	bytesPerSec := int(captureSampleRate) * int(captureChannels) * int(captureBitsPerSample) / 8
	return bytesPerSec * vadFrameMs / 1000
}

// VAD holds the running calibration state for one recording session.
// Create one per recording (don't reuse across hotkey presses — the
// ambient noise floor may have changed).
type VAD struct {
	// noiseFloor is the median RMS of the calibration frames. Set once
	// after `calibrationFrames` frames have been observed.
	noiseFloor float64

	// threshold is the RMS value above which a frame is considered
	// speech. Computed as max(noiseFloor * thresholdMultiplier, minThreshold).
	threshold float64

	// calibrationSamples accumulates RMS values during the first few
	// frames. We use the median rather than the mean to be robust against
	// a single loud transient (door slam, throat clear) blowing up the
	// calibration.
	calibrationSamples []float64

	// calibrated flips true once we've collected enough calibration
	// frames. Until then, IsSpeech returns false (we err on the side of
	// "silence" during calibration so we don't accidentally cut a chunk
	// at frame 1).
	calibrated bool

	// calibrationSuspect is set when the measured noise floor exceeded
	// noiseFloorCeiling — i.e. calibration almost certainly caught speech
	// instead of ambient noise, and we fell back to minThreshold. Exposed
	// for diagnostic logging so the user can spot "I talked too soon" cases.
	calibrationSuspect bool
}

const (
	// calibrationFrames = number of 20ms frames used to estimate noise.
	// 10 frames = 200 ms, enough to characterize ambient noise without
	// making the user wait a perceptible amount of time before VAD
	// starts working.
	calibrationFrames = 10

	// thresholdMultiplier sets how much louder than the noise floor a
	// frame must be to count as speech. 3× is a common heuristic; lower
	// values catch more (including breath/clicks), higher values miss
	// quiet speech.
	thresholdMultiplier = 3.0

	// minThreshold is the lower bound on the speech threshold, so a
	// dead-silent room (noise floor near 0) doesn't give us a threshold
	// of 0 (everything = speech). Tuned against the int16 amplitude
	// scale: roughly 0.3% of full scale.
	minThreshold = 100.0

	// noiseFloorCeiling is the RMS above which we refuse to believe a
	// calibration result is really the ambient noise floor. Calibration
	// assumes the first 200 ms of a press is silence; if the user starts
	// talking immediately, it instead measures their voice, computes a
	// speech-level "floor," and sets the threshold so high it swallows the
	// rest of the utterance (worst case: nothing is transcribed at all).
	//
	// Spoken-voice frames sit in the low thousands of RMS; a quiet dictation
	// environment's ambient noise is tens to low hundreds. 1500 sits well
	// above any realistic ambient floor but below conversational speech, so
	// it trips only when calibration genuinely caught the user's voice. It's
	// a conservative guess pending real-world data — the recorder logs the
	// measured floor every press so this can be tuned later.
	noiseFloorCeiling = 1500.0
)

// NewVAD returns a fresh detector. Call ProcessFrame on each 20ms frame.
func NewVAD() *VAD {
	return &VAD{
		calibrationSamples: make([]float64, 0, calibrationFrames),
	}
}

// IsSpeech returns whether the given 20ms frame contains speech.
// During the calibration phase (first ~200ms), always returns false
// — the recorder treats this as "warming up" and won't emit a chunk
// boundary before VAD is calibrated.
func (v *VAD) IsSpeech(frame []byte) bool {
	rms := frameRMS(frame)

	if !v.calibrated {
		v.calibrationSamples = append(v.calibrationSamples, rms)
		if len(v.calibrationSamples) >= calibrationFrames {
			v.noiseFloor = median(v.calibrationSamples)
			if v.noiseFloor > noiseFloorCeiling {
				// The "ambient" measurement is implausibly loud — the user
				// almost certainly spoke during the 200 ms calibration
				// window, so this isn't a real noise floor. Trusting it would
				// set the threshold above their own voice and swallow the
				// utterance. Fall back to the minimum threshold: we'd rather
				// under-segment (treat borderline frames as speech, yielding
				// one big chunk) than lose audio entirely. noiseFloor keeps
				// the raw measured value so the log can report what happened.
				v.threshold = minThreshold
				v.calibrationSuspect = true
			} else {
				v.threshold = math.Max(v.noiseFloor*thresholdMultiplier, minThreshold)
			}
			v.calibrated = true
		}
		return false
	}

	return rms > v.threshold
}

// Threshold returns the current speech threshold (for logging/debug).
func (v *VAD) Threshold() float64 { return v.threshold }

// NoiseFloor returns the calibrated noise floor RMS (for logging/debug).
func (v *VAD) NoiseFloor() float64 { return v.noiseFloor }

// Calibrated reports whether calibration has completed.
func (v *VAD) Calibrated() bool { return v.calibrated }

// CalibrationSuspect reports whether calibration was rejected as
// speech-contaminated (measured noise floor above noiseFloorCeiling), in
// which case the threshold fell back to minThreshold. For diagnostics.
func (v *VAD) CalibrationSuspect() bool { return v.calibrationSuspect }

// frameRMS computes the root-mean-square amplitude of a 16-bit LE PCM frame.
// Returns a value in the int16 amplitude range (0 to ~32767).
func frameRMS(frame []byte) float64 {
	if len(frame) < 2 {
		return 0
	}
	var sumSq float64
	count := len(frame) / 2
	for i := 0; i+1 < len(frame); i += 2 {
		s := int16(binary.LittleEndian.Uint16(frame[i : i+2]))
		sumSq += float64(s) * float64(s)
	}
	return math.Sqrt(sumSq / float64(count))
}

// median returns the middle value of values (sorted-by-copy so the input
// slice isn't mutated). For an even-length input we return the lower
// midpoint to keep this dependency-free — strict median across an even
// list isn't worth the math package import.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	// Tiny n (we only ever feed it ~10 values), so an insertion sort is
	// fine and avoids pulling in sort.Float64s.
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	return sorted[len(sorted)/2]
}
