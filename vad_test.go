// vad_test.go: unit tests for the energy-based VAD, focused on the
// calibration logic and the speech-contamination guard (noiseFloorCeiling).
//
// These are pure-computation tests — no microphone, no Windows APIs — so
// they run anywhere `go test` runs. We build synthetic frames whose RMS we
// can predict exactly: a frame filled with a single constant int16 sample
// value `s` has RMS == |s| (every sample contributes s², so the mean of the
// squares is s² and its square root is |s|).

package main

import (
	"encoding/binary"
	"math"
	"testing"
)

// constFrame returns one VAD-sized frame whose samples are all `sample`,
// giving it a known RMS of |sample|.
func constFrame(sample int16) []byte {
	n := FrameBytes()
	b := make([]byte, n)
	for i := 0; i+1 < n; i += 2 {
		binary.LittleEndian.PutUint16(b[i:i+2], uint16(sample))
	}
	return b
}

// feed pushes n copies of one constant frame through the detector.
func feed(v *VAD, sample int16, n int) {
	frame := constFrame(sample)
	for i := 0; i < n; i++ {
		v.IsSpeech(frame)
	}
}

const floatTol = 0.5

// A normal quiet room: calibration sees low-amplitude ambient frames, sets a
// sane noise floor, and is NOT flagged suspect. Speech is then detected,
// quiet is not.
func TestVADCalibratesOnQuietRoom(t *testing.T) {
	v := NewVAD()

	// First calibrationFrames frames must all report "silence" (warming up).
	quiet := constFrame(40)
	for i := 0; i < calibrationFrames; i++ {
		if v.IsSpeech(quiet) {
			t.Fatalf("frame %d during calibration reported speech; want silence", i)
		}
	}

	if !v.Calibrated() {
		t.Fatal("VAD should be calibrated after calibrationFrames frames")
	}
	if v.CalibrationSuspect() {
		t.Fatal("quiet-room calibration should not be flagged suspect")
	}
	if math.Abs(v.NoiseFloor()-40) > floatTol {
		t.Fatalf("noise floor = %.2f; want ~40", v.NoiseFloor())
	}
	wantThreshold := math.Max(40*thresholdMultiplier, minThreshold) // 120
	if math.Abs(v.Threshold()-wantThreshold) > floatTol {
		t.Fatalf("threshold = %.2f; want %.2f", v.Threshold(), wantThreshold)
	}

	// Post-calibration classification.
	if !v.IsSpeech(constFrame(3000)) {
		t.Error("loud frame (3000) should be detected as speech")
	}
	if v.IsSpeech(constFrame(60)) {
		t.Error("quiet frame (60) below threshold should be silence")
	}
}

// The bug this finding is about: the user starts talking during the 200 ms
// calibration window, so calibration measures speech, not ambient. The guard
// must reject that floor and fall back to minThreshold so the rest of the
// utterance is still detectable (audio not lost).
func TestVADRejectsSpeechContaminatedCalibration(t *testing.T) {
	v := NewVAD()
	feed(v, 3000, calibrationFrames) // "ambient" = full-voice frames

	if !v.Calibrated() {
		t.Fatal("VAD should be calibrated")
	}
	if !v.CalibrationSuspect() {
		t.Fatalf("noise floor %.0f exceeds ceiling %.0f; should be flagged suspect",
			v.NoiseFloor(), noiseFloorCeiling)
	}
	if math.Abs(v.Threshold()-minThreshold) > floatTol {
		t.Fatalf("suspect calibration threshold = %.2f; want fallback %.2f",
			v.Threshold(), minThreshold)
	}
	// Raw measured floor is retained for diagnostics.
	if math.Abs(v.NoiseFloor()-3000) > floatTol {
		t.Fatalf("noise floor = %.2f; want raw measured ~3000", v.NoiseFloor())
	}
	// The crucial property: real speech is still detected (not swallowed).
	if !v.IsSpeech(constFrame(3000)) {
		t.Error("with fallback threshold, genuine speech must still register")
	}
}

// The median makes calibration robust to a minority of contaminated frames:
// 4 loud frames out of 10 should not move the floor off the quiet value.
func TestVADCalibrationMedianRobustToMinoritySpeech(t *testing.T) {
	v := NewVAD()
	feed(v, 40, 6)   // 6 ambient frames
	feed(v, 3000, 4) // 4 speech frames — minority

	if !v.Calibrated() {
		t.Fatal("VAD should be calibrated after 10 frames")
	}
	if v.CalibrationSuspect() {
		t.Error("minority speech should not trip the suspect guard")
	}
	if math.Abs(v.NoiseFloor()-40) > floatTol {
		t.Fatalf("noise floor = %.2f; want ~40 (median ignores the loud minority)", v.NoiseFloor())
	}
}
