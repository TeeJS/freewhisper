// audiodevices.go: enumerate WASAPI capture (microphone) devices so the user
// can pick a specific mic in Settings instead of always using the Windows
// default.
//
// Why enumerate-and-match instead of a direct lookup: go-wca's
// IMMDeviceEnumerator.GetDevice(id) is an unimplemented stub (returns
// E_NOTIMPL), so to re-open a device the user saved we list the active capture
// endpoints and match on the device ID — the stable string Windows assigns
// each endpoint. Listing for the GUI runs COM on its own short-lived, isolated
// OS thread so it never tangles with walk's message loop or the recorder's COM
// apartment.

package main

import (
	"fmt"
	"log"
	"runtime"

	ole "github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

// captureDevice is one selectable microphone: its stable Windows ID (what we
// persist in config) and a human-friendly name (what we show in the dropdown).
type captureDevice struct {
	ID   string
	Name string
}

// listCaptureDevices returns the active capture (microphone) endpoints. It
// runs all COM work on a dedicated locked OS thread with its own
// CoInitialize/CoUninitialize, so it's safe to call from the settings-dialog
// goroutine (which walk owns) without disturbing anyone's COM apartment.
func listCaptureDevices() ([]captureDevice, error) {
	type result struct {
		devs []captureDevice
		err  error
	}
	ch := make(chan result, 1)

	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
			// Code 1 == S_FALSE == "already initialized on this thread", which
			// is fine; anything else is a genuine failure.
			if oerr, ok := err.(*ole.OleError); !ok || oerr.Code() != 1 {
				ch <- result{nil, fmt.Errorf("CoInitializeEx: %w", err)}
				return
			}
		}
		defer ole.CoUninitialize()

		var mmde *wca.IMMDeviceEnumerator
		if err := wca.CoCreateInstance(
			wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
			wca.IID_IMMDeviceEnumerator, &mmde,
		); err != nil {
			ch <- result{nil, fmt.Errorf("CoCreateInstance(MMDeviceEnumerator): %w", err)}
			return
		}
		defer mmde.Release()

		var collection *wca.IMMDeviceCollection
		if err := mmde.EnumAudioEndpoints(uint32(wca.ECapture), wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
			ch <- result{nil, fmt.Errorf("EnumAudioEndpoints: %w", err)}
			return
		}
		defer collection.Release()

		var count uint32
		if err := collection.GetCount(&count); err != nil {
			ch <- result{nil, fmt.Errorf("GetCount: %w", err)}
			return
		}

		var out []captureDevice
		for i := uint32(0); i < count; i++ {
			var dev *wca.IMMDevice
			if err := collection.Item(i, &dev); err != nil {
				continue
			}
			id, name := deviceIDAndName(dev)
			dev.Release()
			if id != "" {
				out = append(out, captureDevice{ID: id, Name: name})
			}
		}
		ch <- result{out, nil}
	}()

	r := <-ch
	return r.devs, r.err
}

// deviceIDAndName reads an IMMDevice's stable ID and friendly name. Best
// effort: if the property store or name lookup fails we still return the ID so
// the caller can fall back to showing it.
func deviceIDAndName(dev *wca.IMMDevice) (id, name string) {
	dev.GetId(&id)

	var ps *wca.IPropertyStore
	if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return id, ""
	}
	defer ps.Release()

	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return id, ""
	}
	// PROPVARIANT.String() decodes the UTF-16 name and frees the COM memory.
	return id, pv.String()
}

// acquireCaptureDevice returns the IMMDevice the recorder should capture from,
// given the user's configured device ID (empty = system default). It must be
// called on a thread that already has COM initialized (the recorder's capture
// loop). The caller owns the returned device and must Release it.
//
// If a specific device is configured but can't be found (unplugged, renamed,
// disabled), we log it and fall back to the system default rather than fail —
// losing dictation because a USB mic was unplugged would be a bad trade.
func acquireCaptureDevice(mmde *wca.IMMDeviceEnumerator, wantID string) (*wca.IMMDevice, error) {
	if wantID != "" {
		dev, err := findCaptureDeviceByID(mmde, wantID)
		if err != nil {
			log.Printf("mic: enumerating devices failed (%v); using system default", err)
		} else if dev != nil {
			return dev, nil
		} else {
			log.Print("mic: configured device not found; using system default")
		}
	}

	var dev *wca.IMMDevice
	if err := mmde.GetDefaultAudioEndpoint(uint32(wca.ECapture), 0, &dev); err != nil {
		return nil, fmt.Errorf("GetDefaultAudioEndpoint(eCapture, eConsole): %w", err)
	}
	return dev, nil
}

// findCaptureDeviceByID enumerates active capture endpoints with the given
// (already COM-initialized) enumerator and returns the one whose ID matches,
// or (nil, nil) if none match. The caller owns the returned device.
func findCaptureDeviceByID(mmde *wca.IMMDeviceEnumerator, wantID string) (*wca.IMMDevice, error) {
	var collection *wca.IMMDeviceCollection
	if err := mmde.EnumAudioEndpoints(uint32(wca.ECapture), wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return nil, err
	}
	defer collection.Release()

	var count uint32
	if err := collection.GetCount(&count); err != nil {
		return nil, err
	}
	for i := uint32(0); i < count; i++ {
		var dev *wca.IMMDevice
		if err := collection.Item(i, &dev); err != nil {
			continue
		}
		var id string
		dev.GetId(&id)
		if id == wantID {
			return dev, nil // caller releases
		}
		dev.Release()
	}
	return nil, nil
}

// volScalar converts a 0–100 percent to the 0.0–1.0 scalar WASAPI's
// IAudioEndpointVolume expects, clamping out-of-range input.
func volScalar(pct int) float32 {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return float32(pct) / 100
}

// setDeviceVolume sets an already-acquired capture device's master input level
// (0–100%). Called from the recorder's COM context, where it already holds the
// device. This is the *Windows* mic level — system-wide, the same slider you'd
// move in Sound settings — so it affects every app, by design.
func setDeviceVolume(device *wca.IMMDevice, pct int) error {
	var aev *wca.IAudioEndpointVolume
	if err := device.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil {
		return fmt.Errorf("Activate(IAudioEndpointVolume): %w", err)
	}
	defer aev.Release()
	return aev.SetMasterVolumeLevelScalar(volScalar(pct), nil)
}

// withEndpointVolume resolves the capture device (configured ID, or the Windows
// default when empty/missing) on its own isolated COM thread, activates
// IAudioEndpointVolume, and runs fn. Used by the settings dialog to read and
// apply the level without touching walk's or the recorder's COM apartment.
func withEndpointVolume(deviceID string, fn func(aev *wca.IAudioEndpointVolume) error) error {
	ch := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
			if oerr, ok := err.(*ole.OleError); !ok || oerr.Code() != 1 {
				ch <- fmt.Errorf("CoInitializeEx: %w", err)
				return
			}
		}
		defer ole.CoUninitialize()

		var mmde *wca.IMMDeviceEnumerator
		if err := wca.CoCreateInstance(
			wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
			wca.IID_IMMDeviceEnumerator, &mmde,
		); err != nil {
			ch <- fmt.Errorf("CoCreateInstance(MMDeviceEnumerator): %w", err)
			return
		}
		defer mmde.Release()

		device, err := acquireCaptureDevice(mmde, deviceID)
		if err != nil {
			ch <- err
			return
		}
		defer device.Release()

		var aev *wca.IAudioEndpointVolume
		if err := device.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil {
			ch <- fmt.Errorf("Activate(IAudioEndpointVolume): %w", err)
			return
		}
		defer aev.Release()

		ch <- fn(aev)
	}()
	return <-ch
}

// applyCaptureVolume sets the input level (0–100%) of the given device now.
func applyCaptureVolume(deviceID string, pct int) error {
	return withEndpointVolume(deviceID, func(aev *wca.IAudioEndpointVolume) error {
		return aev.SetMasterVolumeLevelScalar(volScalar(pct), nil)
	})
}

// readCaptureVolume returns the device's current input level as 0–100%.
func readCaptureVolume(deviceID string) (int, error) {
	var pct int
	err := withEndpointVolume(deviceID, func(aev *wca.IAudioEndpointVolume) error {
		var s float32
		if err := aev.GetMasterVolumeLevelScalar(&s); err != nil {
			return err
		}
		pct = int(s*100 + 0.5) // round to nearest percent
		return nil
	})
	return pct, err
}
