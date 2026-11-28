// Package quality is the remote desktop's picture quality presets
// (DesktopQuality): what each costs on the link, and how "auto" moves
// between them. It has no dependencies, so viewers (the phone's core
// included) can list the presets.
package quality

import (
	"fmt"
	"slices"
)

// Level is one step of picture quality.
type Level struct {
	Name  string
	Label string // for menus
	// MaxHeight scales the picture down to this many lines (0: the
	// display's size); FPS caps the frame rate (0: the viewer's max_fps);
	// Kbps is the encoder's bitrate (0: from the picture's size).
	MaxHeight, FPS, Kbps int
}

// Levels, best first: "auto" moves along them.
var Levels = []Level{
	{Name: "high", Label: "High (full size and frame rate)"},
	{Name: "balanced", Label: "Balanced (1080p, 30 fps, 4 Mbit/s)", MaxHeight: 1080, FPS: 30, Kbps: 4000},
	{Name: "low", Label: "Low bandwidth (720p, 15 fps, 1 Mbit/s)", MaxHeight: 720, FPS: 15, Kbps: 1000},
	{Name: "minimal", Label: "Minimal (540p, 8 fps, 300 kbit/s)", MaxHeight: 540, FPS: 8, Kbps: 300},
}

// Auto is the preset that follows the link.
const Auto = "auto"

// AutoLabel is Auto's menu label.
const AutoLabel = "Auto (follows the connection)"

// Presets: the names a viewer may ask for, in menu order.
func Presets() []string {
	out := []string{Auto}
	for _, l := range Levels {
		out = append(out, l.Name)
	}
	return out
}

// Label is a preset's menu label.
func Label(preset string) string {
	if preset == Auto {
		return AutoLabel
	}
	if l, ok := Find(preset); ok {
		return l.Label
	}
	return preset
}

// Find is the level of that name.
func Find(name string) (Level, bool) {
	i := slices.IndexFunc(Levels, func(l Level) bool { return l.Name == name })
	if i < 0 {
		return Level{}, false
	}
	return Levels[i], true
}

// Valid: preset is "" (the default), Auto or a level.
func Valid(preset string) error {
	if preset == "" || preset == Auto {
		return nil
	}
	if _, ok := Find(preset); ok {
		return nil
	}
	return fmt.Errorf("unknown quality %q (want one of %v)", preset, Presets())
}

// Size is w x h scaled down to at most maxHeight lines (0: unchanged),
// even, keeping the aspect.
func Size(w, h, maxHeight int) (int, int) {
	if maxHeight <= 0 || h <= maxHeight {
		return w &^ 1, h &^ 1
	}
	nw := w * maxHeight / h
	return max(nw&^1, 2), maxHeight &^ 1
}

// Adapter is "auto": it watches how long the sender waits for the
// viewer's acks and moves a level down when the link can't keep up, back
// up after it kept up a while.
type Adapter struct {
	level        int     // index in Levels
	busy, window float64 // seconds waited for acks, seconds watched, this round
	calm         float64 // seconds the link kept up, since the last move
}

// Thresholds: a round is this long; more than downAt of it waiting means
// the link is the limit; a level up after upAfter of no more than upAt.
const (
	round   = 3.0
	downAt  = 0.35
	upAt    = 0.05
	upAfter = 15.0
)

// NewAdapter starts at "balanced": most links carry it, and the first
// rounds tell.
func NewAdapter() *Adapter { return &Adapter{level: 1} }

// Level is the level in force.
func (a *Adapter) Level() Level { return Levels[a.level] }

// Observe adds elapsed seconds of which waited were spent waiting for
// acks; true: the level changed (Level).
func (a *Adapter) Observe(elapsed, waited float64) bool {
	a.window += elapsed
	a.busy += waited
	if a.window < round {
		return false
	}
	share := a.busy / a.window
	a.window, a.busy = 0, 0
	switch {
	case share > downAt && a.level < len(Levels)-1:
		a.level++
		a.calm = 0
		return true
	case share <= upAt:
		a.calm += round
		if a.calm >= upAfter && a.level > 0 {
			a.level--
			a.calm = 0
			return true
		}
	default:
		a.calm = 0
	}
	return false
}
