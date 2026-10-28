// Package tray is longarm-tray's toolkit-independent part: the loopback server
// that serves the shared web UI and proxies the local control APIs, the
// tray icon renderer, the notification rules, and the controller that
// follows longarm daemon's pool view. cmd/longarm-tray adds the desktop shell (Wails).
package tray

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/machine"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// LongOffline: a worker offline (by the hub's last_seen) for longer is an
// old registration: left out of the icon and the tooltip, as the popup
// collapses it into its "Offline" section.
var LongOffline = time.Hour

// machineWorkers are v's workers as machines: an admin worker that pairs
// with a listed session worker (package machine) is left out - its machine
// is the session worker's. longOffline also leaves out workers offline for
// more than LongOffline.
func machineWorkers(v *pb.PoolView, longOffline bool) []*pb.PoolWorker {
	var nodes []*pb.NodeInfo
	for _, w := range v.GetWorkers() {
		if w.GetNode() != nil {
			nodes = append(nodes, w.GetNode())
		}
	}
	pairs := machine.Pairs(nodes)
	now := time.Now()
	if t := v.GetTime(); t != nil {
		now = t.AsTime()
	}
	var out []*pb.PoolWorker
	for _, w := range v.GetWorkers() {
		if _, ok := pairs[w.Name]; ok {
			continue
		}
		if seen := w.GetNode().GetLastSeen(); longOffline && w.State == "offline" && seen != nil && now.Sub(seen.AsTime()) > LongOffline {
			continue
		}
		out = append(out, w)
	}
	return out
}

// Palette.
var (
	colBG     = color.RGBA{13, 17, 23, 255}
	colBorder = color.RGBA{48, 54, 61, 255}
	colTrack  = color.RGBA{33, 38, 45, 255}
	colGreen  = color.RGBA{63, 185, 80, 255}
	colAmber  = color.RGBA{210, 153, 34, 255}
	colRed    = color.RGBA{248, 81, 73, 255}
	colMuted  = color.RGBA{139, 148, 158, 255}
)

// IconWorker is what the icon shows of one worker.
type IconWorker struct {
	State string  // online | connecting | offline
	CPU   float64 // percent
	Mem   float64 // percent
	Valid bool    // metrics known
}

// IconWorkers maps a pool view to icon columns (in the view's order).
func IconWorkers(v *pb.PoolView) []IconWorker {
	var out []IconWorker
	for _, w := range machineWorkers(v, true) {
		iw := IconWorker{State: w.State}
		if m := w.Metrics; m != nil && (w.State == "online" || w.Source == "hub") {
			iw.Valid, iw.CPU = true, m.CpuPercent
			if m.MemTotalBytes > 0 {
				iw.Mem = 100 * float64(m.MemUsedBytes) / float64(m.MemTotalBytes)
			}
		}
		out = append(out, iw)
	}
	return out
}

// RenderIcon draws the 64x64 tray icon: a dark rounded square
// with one column per worker, each a CPU bar (left) and a memory bar
// (right) coloured green / amber / red; an offline worker is a red cross,
// one still connecting a grey cross.
func RenderIcon(ws []IconWorker) *image.RGBA {
	const S = 64
	img := image.NewRGBA(image.Rect(0, 0, S, S))
	roundedRect(img, 0, 0, S-1, S-1, 12, colBG, colBorder, 2)
	n := max(1, len(ws))
	pad, gap := 7.0, 5.0
	if n > 6 { // many workers: tighter
		pad, gap = 5, 2
	}
	colW := (S - 2*pad - gap*float64(n-1)) / float64(n)
	top, bottom := 8.0, float64(S-8)
	for i, w := range ws {
		x0 := pad + float64(i)*(colW+gap)
		bw := (colW - 2) / 2
		if colW < 4 {
			bw = colW / 2
		}
		for j := 0; j < 2; j++ {
			bx := x0 + float64(j)*(bw+2)
			fillRect(img, bx, top, bx+bw, bottom, colTrack)
		}
		if w.State == "online" && w.Valid {
			for j, pct := range []float64{w.CPU, w.Mem} {
				bx := x0 + float64(j)*(bw+2)
				h := max(2, (bottom-top)*math.Min(math.Max(pct, 0), 100)/100)
				fillRect(img, bx, bottom-h, bx+bw, bottom, levelColor(pct))
			}
			continue
		}
		c := colMuted
		if w.State == "offline" {
			c = colRed
		}
		line(img, x0+1, top+14, x0+colW-1, bottom-14, 4, c)
		line(img, x0+colW-1, top+14, x0+1, bottom-14, 4, c)
	}
	return img
}

// IconPNG renders the icon as PNG bytes.
func IconPNG(ws []IconWorker) []byte {
	var b bytes.Buffer
	png.Encode(&b, RenderIcon(ws))
	return b.Bytes()
}

// Tooltip is one line per worker (state, load, the link),
// cut to 127 characters (the Windows limit).
func Tooltip(v *pb.PoolView) string {
	var parts []string
	for _, w := range machineWorkers(v, true) {
		if w.State != "online" {
			st := "offline"
			if w.State == "connecting" {
				st = "connecting"
			}
			parts = append(parts, w.Name+": "+st)
			continue
		}
		s := w.Name + ":"
		if m := w.Metrics; m != nil {
			cc := m.Agents["claude"].GetSessions()
			cp := m.Agents["copilot"].GetSessions()
			s += fmt.Sprintf(" CPU %.0f%% MEM %.0f/%.0fG CC%d CP%d", m.CpuPercent, gib(m.MemUsedBytes), gib(m.MemTotalBytes), cc, cp)
		}
		if l := w.Link; l != nil && l.RttMs > 0 {
			s += fmt.Sprintf(" %.0fms", l.RttMs)
			if a := activeLeg(l); a != nil && a.Kind != "" && a.Kind != "direct" {
				s += " " + a.Kind
			}
		}
		parts = append(parts, s)
	}
	t := strings.Join(parts, "\n")
	if t == "" {
		t = "Remote Workers"
	}
	if r := []rune(t); len(r) > 127 {
		t = string(r[:127])
	}
	return t
}

func gib(b uint64) float64 { return float64(b) / (1 << 30) }

func activeLeg(l *pb.WorkerLink) *pb.LegStatus {
	for _, g := range l.GetLegs() {
		if g.Active {
			return g
		}
	}
	return nil
}

// ---------------------------------------------------------------- drawing

func fillRect(img *image.RGBA, x0, y0, x1, y1 float64, c color.RGBA) {
	for y := int(math.Floor(y0)); y <= int(math.Ceil(y1)); y++ {
		for x := int(math.Floor(x0)); x <= int(math.Ceil(x1)); x++ {
			// coverage-based antialiasing on the edges
			cov := overlap(float64(x), x0, x1+1) * overlap(float64(y), y0, y1+1)
			if cov > 0 {
				blend(img, x, y, c, cov)
			}
		}
	}
}

// overlap is how much of the pixel [p, p+1) lies in [a, b).
func overlap(p, a, b float64) float64 {
	return math.Max(0, math.Min(p+1, b)-math.Max(p, a))
}

func blend(img *image.RGBA, x, y int, c color.RGBA, a float64) {
	if !(image.Point{x, y}.In(img.Rect)) {
		return
	}
	a = math.Min(1, a) * float64(c.A) / 255
	o := img.RGBAAt(x, y)
	oa := float64(o.A) / 255
	na := a + oa*(1-a)
	if na == 0 {
		return
	}
	mix := func(cv, ov uint8) uint8 {
		return uint8(math.Round((float64(cv)*a + float64(ov)*oa*(1-a)) / na))
	}
	img.SetRGBA(x, y, color.RGBA{mix(c.R, o.R), mix(c.G, o.G), mix(c.B, o.B), uint8(math.Round(na * 255))})
}

// roundedRect fills a rounded rectangle with an outline of width w.
func roundedRect(img *image.RGBA, x0, y0, x1, y1, r float64, fill, outline color.RGBA, w float64) {
	cx, cy := (x0+x1+1)/2, (y0+y1+1)/2
	hx, hy := (x1+1-x0)/2, (y1+1-y0)/2
	// inside is the signed distance to the box shrunk by inset (rounded
	// box SDF; negative = inside).
	inside := func(px, py, inset float64) float64 {
		rr := math.Max(0, r-inset)
		qx := math.Abs(px-cx) - (hx - inset - rr)
		qy := math.Abs(py-cy) - (hy - inset - rr)
		return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - rr
	}
	for y := int(y0); y <= int(y1)+1; y++ {
		for x := int(x0); x <= int(x1)+1; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			if a := clamp01(0.5 - inside(px, py, 0)); a > 0 {
				blend(img, x, y, outline, a)
			}
			if a := clamp01(0.5 - inside(px, py, w)); a > 0 {
				blend(img, x, y, fill, a)
			}
		}
	}
}

// line draws a thick antialiased segment.
func line(img *image.RGBA, xa, ya, xb, yb, w float64, c color.RGBA) {
	minX, maxX := int(math.Min(xa, xb)-w), int(math.Max(xa, xb)+w)
	minY, maxY := int(math.Min(ya, yb)-w), int(math.Max(ya, yb)+w)
	dx, dy := xb-xa, yb-ya
	l2 := dx*dx + dy*dy
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			t := 0.0
			if l2 > 0 {
				t = math.Max(0, math.Min(1, ((px-xa)*dx+(py-ya)*dy)/l2))
			}
			d := math.Hypot(px-(xa+t*dx), py-(ya+t*dy))
			if a := clamp01(w/2 + 0.5 - d); a > 0 {
				blend(img, x, y, c, a)
			}
		}
	}
}

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }
