package core

import (
	"errors"
	"strings"

	"longarm/internal/mobile"
	"longarm/internal/rdesk/quality"
)

// DesktopListener is the native viewer (Swift / Kotlin implement it):
// called on a background thread.
type DesktopListener interface {
	// Started: a new picture of width×height, codec "h264"; the next frame
	// is a key frame (with its SPS and PPS).
	Started(width, height int, codec string, displays int)
	// Frame: an H.264 access unit (Annex B); Ack(seq) once it is shown.
	Frame(data []byte, key bool, seq int64)
	// Cursor: where the pointer is; shape (BGRA w×h, hot spot) when it changed (else empty).
	Cursor(x, y int, visible bool, shapeID int64, w, h, hotX, hotY int, bgra []byte)
	// Quality: after Started, the picture quality in force - the preset
	// asked for ("auto", ...; "": the worker has no presets), auto's level
	// now, the frame rate and bitrate.
	Quality(preset, level string, fps, kbps int)
	Note(text string)
	// Ended: the stream is over; err "" when Close ended it.
	Ended(err string)
}

// Desktop is an open remote desktop (OpenDesktop).
type Desktop struct{ d *mobile.Desktop }

type events struct{ l DesktopListener }

func (e events) Started(w, h int, codec string, displays int) { e.l.Started(w, h, codec, displays) }
func (e events) Frame(data []byte, key bool, seq uint64)      { e.l.Frame(data, key, int64(seq)) }
func (e events) Cursor(x, y int, visible bool, shape uint64, w, h, hx, hy int, bgra []byte) {
	e.l.Cursor(x, y, visible, int64(shape), w, h, hx, hy, bgra)
}
func (e events) Quality(preset, level string, fps, kbps int) {
	e.l.Quality(preset, level, fps, kbps)
}
func (e events) Note(text string) { e.l.Note(text) }
func (e events) Ended(err string) { e.l.Ended(err) }

// OpenDesktop opens the desktop of one of the user's machines: maxFPS 0 =
// 30, bitrateKbps 0 = the worker's choice; quality a preset (QualityPresets;
// "auto" follows the connection, "" the worker's default).
func OpenDesktop(worker string, display, maxFPS, bitrateKbps int, quality string, l DesktopListener) (*Desktop, error) {
	mu.Lock()
	c := core
	mu.Unlock()
	if c == nil {
		return nil, errors.New("the core is not running")
	}
	d, err := c.OpenDesktop(worker, mobile.DesktopOptions{Display: display, MaxFPS: maxFPS, BitrateKbps: bitrateKbps, Quality: quality}, events{l})
	if err != nil {
		return nil, err
	}
	return &Desktop{d}, nil
}

func (d *Desktop) Ack(seq int64)                     { d.d.Ack(uint64(seq)) }
func (d *Desktop) Keyframe()                         { d.d.Keyframe() }
func (d *Desktop) Mouse(x, y, button int, down bool) { d.d.Mouse(x, y, button, down) }
func (d *Desktop) Wheel(dx, dy int)                  { d.d.Wheel(dx, dy) }
func (d *Desktop) Key(scan int, down bool)           { d.d.Key(scan, down) }
func (d *Desktop) Text(s string)                     { d.d.Text(s) }
func (d *Desktop) SecureAttention()                  { d.d.SecureAttention() }
func (d *Desktop) Close()                            { d.d.Close() }

// SetQuality asks for another picture quality (a preset).
func (d *Desktop) SetQuality(preset string) error { return d.d.SetQuality(preset) }

// QualityPresets lists the quality presets for a menu: one per line,
// "name<TAB>label", best first after "auto".
func QualityPresets() string {
	var b strings.Builder
	for _, p := range quality.Presets() {
		b.WriteString(p + "\t" + quality.Label(p) + "\n")
	}
	return b.String()
}

// Reconnect drops the core's connections: call it when the app comes back
// to the front (the system may have ended them while it was away).
func Reconnect() {
	mu.Lock()
	c := core
	mu.Unlock()
	if c != nil {
		c.Reconnect()
	}
}
