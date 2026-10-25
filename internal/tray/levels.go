package tray

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"image/color"
	"math"
)

// levelsJSON is the one threshold table for load colours; the web UI
// imports the same file (web/src/lib/format.ts), so the icon and the popup
// can't disagree.
//
//go:embed levels.json
var levelsJSON []byte

type level struct {
	Below float64 // upper limit (exclusive); +Inf for the last level
	Color color.RGBA
}

var levels = mustLevels(levelsJSON)

func mustLevels(b []byte) []level {
	var f struct {
		Levels []struct {
			Below *float64 `json:"below"`
			Color string   `json:"color"`
		} `json:"levels"`
	}
	if err := json.Unmarshal(b, &f); err != nil || len(f.Levels) == 0 {
		panic(fmt.Sprintf("tray: bad levels.json: %v", err))
	}
	out := make([]level, len(f.Levels))
	for i, l := range f.Levels {
		var r, g, bl uint8
		if _, err := fmt.Sscanf(l.Color, "#%02x%02x%02x", &r, &g, &bl); err != nil {
			panic(fmt.Sprintf("tray: levels.json color %q: %v", l.Color, err))
		}
		out[i] = level{Below: math.Inf(1), Color: color.RGBA{r, g, bl, 255}}
		if l.Below != nil {
			out[i].Below = *l.Below
		}
	}
	return out
}

// levelColor is the colour of a load percentage (levels.json).
func levelColor(pct float64) color.RGBA {
	for _, l := range levels {
		if pct < l.Below {
			return l.Color
		}
	}
	return levels[len(levels)-1].Color
}
