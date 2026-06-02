// audiolevel.go: a lightweight microphone level monitor for the settings
// dialog's "test mic" meter. It opens a WASAPI capture stream on a chosen
// device and continuously reports the current peak level (0–100), WITHOUT the
// VAD/chunking machinery of the real recorder. Used only while the user has the
// level meter enabled in Settings, then torn down.
//
// Why an actual capture stream rather than IAudioMeterInformation: the endpoint
// peak meter reads 0 unless a stream is active on the device, so to show a real
// level we have to capture and measure the samples ourselves. The WASAPI setup
// here deliberately mirrors recorder.go's captureLoop (kept separate to avoid
// destabilizing the battle-tested recorder for a diagnostic feature).

package main

import (
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// levelMonitor captures from one device and exposes its current peak level.
type levelMonitor struct {
	level int32         // atomic; current peak as 0–100
	stop  chan struct{} // closed by Stop()
	done  chan struct{} // closed by run() on exit
}

// startLevelMonitor begins capturing from deviceID (empty = system default) and
// reporting the peak level. Call Stop() to end it. If the device can't be
// opened, the monitor just reports 0 until stopped — the meter stays flat, which
// is itself a useful signal that the mic isn't working.
func startLevelMonitor(deviceID string) *levelMonitor {
	m := &levelMonitor{stop: make(chan struct{}), done: make(chan struct{})}
	go m.run(deviceID)
	return m
}

// Level returns the most recent peak level, 0–100.
func (m *levelMonitor) Level() int { return int(atomic.LoadInt32(&m.level)) }

// Stop ends the monitor and waits for its capture thread to finish and release
// the device.
func (m *levelMonitor) Stop() {
	close(m.stop)
	<-m.done
}

func (m *levelMonitor) run(deviceID string) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(m.done)

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		if oerr, ok := err.(*ole.OleError); !ok || oerr.Code() != 1 {
			return
		}
	}
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		return
	}
	defer mmde.Release()

	device, err := acquireCaptureDevice(mmde, deviceID)
	if err != nil {
		return
	}
	defer device.Release()

	var audioClient *wca.IAudioClient
	if err := device.Activate(wca.IID_IAudioClient, wca.CLSCTX_ALL, nil, &audioClient); err != nil {
		return
	}
	defer audioClient.Release()

	blockAlign := captureChannels * captureBitsPerSample / 8
	wfx := &wca.WAVEFORMATEX{
		WFormatTag:      wca.WAVE_FORMAT_PCM,
		NChannels:       captureChannels,
		NSamplesPerSec:  captureSampleRate,
		NAvgBytesPerSec: captureSampleRate * uint32(blockAlign),
		NBlockAlign:     blockAlign,
		WBitsPerSample:  captureBitsPerSample,
	}
	flags := uint32(wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY)
	if err := audioClient.Initialize(
		wca.AUDCLNT_SHAREMODE_SHARED, flags,
		wca.REFERENCE_TIME(10_000_000), 0, wfx, nil,
	); err != nil {
		return
	}

	var captureClient *wca.IAudioCaptureClient
	if err := audioClient.GetService(wca.IID_IAudioCaptureClient, &captureClient); err != nil {
		return
	}
	defer captureClient.Release()

	if err := audioClient.Start(); err != nil {
		return
	}
	defer audioClient.Stop()

	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			peak := drainPeak(captureClient, int(wfx.NBlockAlign))
			// Smooth the fall so the bar eases down between words instead of
			// snapping to zero; rises track the peak immediately.
			cur := atomic.LoadInt32(&m.level)
			if peak >= cur {
				atomic.StoreInt32(&m.level, peak)
			} else {
				atomic.StoreInt32(&m.level, cur-(cur-peak)/2)
			}
		}
	}
}

// drainPeak pulls all currently-available capture packets and returns the
// loudest sample as a 0–100 value. blockAlign is bytes per frame.
func drainPeak(cc *wca.IAudioCaptureClient, blockAlign int) int32 {
	maxAbs := 0
	for {
		var packetFrames uint32
		if err := cc.GetNextPacketSize(&packetFrames); err != nil || packetFrames == 0 {
			break
		}
		var dataPtr *byte
		var framesRead, flags uint32
		if err := cc.GetBuffer(&dataPtr, &framesRead, &flags, nil, nil); err != nil {
			break
		}
		if framesRead > 0 && flags&audClntBufferFlagsSilent == 0 {
			n := int(framesRead) * blockAlign
			buf := unsafe.Slice(dataPtr, n)
			for i := 0; i+1 < n; i += 2 {
				s := int16(uint16(buf[i]) | uint16(buf[i+1])<<8)
				a := int(s)
				if a < 0 {
					a = -a
				}
				if a > maxAbs {
					maxAbs = a
				}
			}
		}
		cc.ReleaseBuffer(framesRead)
	}
	pct := maxAbs * 100 / 32767 // int16 full scale → 100
	if pct > 100 {
		pct = 100
	}
	return int32(pct)
}
