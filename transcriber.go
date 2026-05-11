// transcriber.go: minimal Wyoming-protocol client for faster-whisper.
//
// Wyoming is the protocol used by Home Assistant's voice pipeline, the
// Rhasspy stack, and many self-hosted "voice satellite" projects. It's
// JSONL + binary payloads over plain TCP. There is no official Go library,
// but the protocol is small enough that we implement it inline here.
//
// Wire format (per event):
//
//	<header-json>\n
//	<data-json bytes>      (only if header.data_length > 0)
//	<binary payload bytes> (only if header.payload_length > 0)
//
// The header is a single line of JSON terminated by '\n'. It always has a
// "type" field, plus optional "data" (inline JSON object), "data_length"
// (size of a separate JSON-data block that follows), and "payload_length"
// (size of a binary payload block that follows after the data block).
//
// The faster-whisper transcription dance we implement here:
//
//  1. → transcribe        — "I want a transcript. Language: en."
//  2. → audio-start       — "Audio is coming. Rate: 48000, mono, 16-bit."
//  3. → audio-chunk × N   — chunks of PCM (binary payload)
//  4. → audio-stop        — "I'm done sending audio. Please transcribe."
//  5. ← transcript        — "Here's the text the user said."
//
// The server tolerates any sample rate and resamples internally to 16 kHz
// (whisper's training rate), so we don't bother downsampling client-side.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"time"
)

// wyomingHeader is the per-event JSON line. We use json.RawMessage for
// data so we can either inline a small struct or omit it entirely without
// fighting the encoder.
type wyomingHeader struct {
	Type          string          `json:"type"`
	Data          json.RawMessage `json:"data,omitempty"`
	DataLength    int             `json:"data_length,omitempty"`
	PayloadLength int             `json:"payload_length,omitempty"`
}

// Transcribe sends pcm audio (48 kHz mono 16-bit signed, little-endian — the
// format produced by recorder.go) to the Wyoming whisper server at endpoint
// and returns the recognized text.
//
// endpoint format: "host:port" (no scheme). Example: "192.168.1.25:10300".
// language: BCP-47-ish code, e.g. "en". Empty for auto-detect.
//
// Network errors and unexpected responses propagate as Go errors so the
// caller (main.go) can log them — we never panic, even on garbage from the
// server, because the whole loop has to keep running for the next press.
func Transcribe(endpoint, language string, pcm []byte) (string, error) {
	// Generous timeouts: dialing on home LAN should be instant, but if
	// the user's whisper container is loading a large model on first
	// request, transcription itself can take several seconds. 30s gives
	// plenty of headroom for "medium" model on an RTX 3060.
	conn, err := net.DialTimeout("tcp", endpoint, 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", endpoint, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return "", fmt.Errorf("set deadline: %w", err)
	}

	// --- Step 1: transcribe ---
	// We send this first event with the language we want. The server
	// remembers the language for this connection and uses it for the
	// next audio session.
	if err := sendEvent(conn, "transcribe", map[string]any{
		"language": language,
	}, nil); err != nil {
		return "", fmt.Errorf("send transcribe: %w", err)
	}

	// --- Step 2: audio-start ---
	// Declare the format of audio we're about to send. The server uses
	// these numbers to drive its internal resampler.
	if err := sendEvent(conn, "audio-start", map[string]any{
		"rate":     int(captureSampleRate),    // 48000
		"width":    int(captureBitsPerSample) / 8, // bytes per sample = 2
		"channels": int(captureChannels),       // 1
	}, nil); err != nil {
		return "", fmt.Errorf("send audio-start: %w", err)
	}

	// --- Step 3: audio-chunk × N ---
	// Split the recording into ~50 ms chunks. There's no firm rule for
	// chunk size in Wyoming — but very small (single sample) is wasteful
	// (one header per byte) and very large (whole recording) bloats
	// memory and offers no streaming advantage. ~50 ms is the size other
	// Wyoming clients use and what the server is well-tested against.
	//
	// 50 ms at 48 kHz mono 16-bit = 4800 bytes. Round to nearest frame
	// boundary (blockAlign = 2) — it already is, but we compute it
	// generically in case constants change.
	const chunkDurationMs = 50
	bytesPerSecond := int(captureSampleRate) * int(captureChannels) * int(captureBitsPerSample) / 8
	chunkBytes := (bytesPerSecond * chunkDurationMs / 1000)
	blockAlign := int(captureChannels) * int(captureBitsPerSample) / 8
	chunkBytes -= chunkBytes % blockAlign // align down to whole frames

	for off := 0; off < len(pcm); off += chunkBytes {
		end := off + chunkBytes
		if end > len(pcm) {
			end = len(pcm)
		}
		chunk := pcm[off:end]
		// Same rate/width/channels payload as audio-start — the server
		// expects each chunk to reassert its format (it's how the wire
		// protocol is specified, not negotiable).
		if err := sendEvent(conn, "audio-chunk", map[string]any{
			"rate":     int(captureSampleRate),
			"width":    int(captureBitsPerSample) / 8,
			"channels": int(captureChannels),
		}, chunk); err != nil {
			return "", fmt.Errorf("send audio-chunk @ offset %d: %w", off, err)
		}
	}

	// --- Step 4: audio-stop ---
	// "Done sending. Please transcribe what I sent."
	if err := sendEvent(conn, "audio-stop", nil, nil); err != nil {
		return "", fmt.Errorf("send audio-stop: %w", err)
	}

	// --- Step 5: read response ---
	// The server may emit multiple events (e.g., progress notifications).
	// We read events until we get a "transcript" type. Anything else gets
	// quietly ignored. If the connection closes with no transcript, that's
	// an error — partial transcript would be better than nothing but
	// faster-whisper doesn't stream partials over Wyoming, only finals.
	reader := bufio.NewReader(conn)
	for {
		evt, data, _, err := readEvent(reader)
		if err != nil {
			return "", fmt.Errorf("read response: %w", err)
		}
		if evt.Type != "transcript" {
			continue // not the event we want; keep reading
		}
		// transcript event puts the recognized text in data.text
		var t struct {
			Text string `json:"text"`
		}
		// Wyoming can put the JSON inline in the header's "data" field OR
		// in a separate "data_length"-block. Handle both. (faster-whisper
		// uses the separate block for transcripts because it can be long.)
		if len(evt.Data) > 0 {
			if err := json.Unmarshal(evt.Data, &t); err != nil {
				return "", fmt.Errorf("parse inline transcript: %w", err)
			}
		} else if len(data) > 0 {
			if err := json.Unmarshal(data, &t); err != nil {
				return "", fmt.Errorf("parse transcript data block: %w", err)
			}
		}
		return t.Text, nil
	}
}

// sendEvent encodes one Wyoming event onto the connection. dataObj is the
// inline "data" object (or nil for none); payload is binary bytes (or nil).
//
// For brevity we never use the separate data-block form when sending —
// inlining keeps things tidy and the server accepts both forms.
func sendEvent(w io.Writer, eventType string, dataObj map[string]any, payload []byte) error {
	hdr := wyomingHeader{Type: eventType}
	if dataObj != nil {
		raw, err := json.Marshal(dataObj)
		if err != nil {
			return fmt.Errorf("marshal data: %w", err)
		}
		hdr.Data = raw
	}
	if len(payload) > 0 {
		hdr.PayloadLength = len(payload)
	}
	headerJSON, err := json.Marshal(hdr)
	if err != nil {
		return fmt.Errorf("marshal header: %w", err)
	}
	// header line + newline
	if _, err := w.Write(append(headerJSON, '\n')); err != nil {
		return fmt.Errorf("write header: %w", err)
	}
	// binary payload (if any)
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return fmt.Errorf("write payload: %w", err)
		}
	}
	return nil
}

// readEvent reads one Wyoming event from r and returns:
//   - the parsed header
//   - the data-block bytes (if data_length > 0; else nil)
//   - the payload-block bytes (if payload_length > 0; else nil)
//
// On any malformed event or premature EOF, returns the error.
func readEvent(r *bufio.Reader) (wyomingHeader, []byte, []byte, error) {
	var hdr wyomingHeader
	line, err := r.ReadString('\n')
	if err != nil {
		return hdr, nil, nil, fmt.Errorf("read header line: %w", err)
	}
	if err := json.Unmarshal([]byte(line), &hdr); err != nil {
		return hdr, nil, nil, fmt.Errorf("parse header (%q): %w", line, err)
	}
	var dataBlock []byte
	if hdr.DataLength > 0 {
		dataBlock = make([]byte, hdr.DataLength)
		if _, err := io.ReadFull(r, dataBlock); err != nil {
			return hdr, nil, nil, fmt.Errorf("read data block: %w", err)
		}
	}
	var payload []byte
	if hdr.PayloadLength > 0 {
		payload = make([]byte, hdr.PayloadLength)
		if _, err := io.ReadFull(r, payload); err != nil {
			return hdr, nil, nil, fmt.Errorf("read payload: %w", err)
		}
	}
	return hdr, dataBlock, payload, nil
}
