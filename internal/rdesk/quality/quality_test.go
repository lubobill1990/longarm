package quality

import "testing"

func TestSize(t *testing.T) {
	for _, c := range []struct{ w, h, max, ww, wh int }{
		{1920, 1080, 0, 1920, 1080},
		{1920, 1080, 1080, 1920, 1080},
		{1920, 1080, 720, 1280, 720},
		{2560, 1440, 540, 960, 540},
		{1366, 768, 720, 1280, 720},
		{1281, 721, 0, 1280, 720},
	} {
		if w, h := Size(c.w, c.h, c.max); w != c.ww || h != c.wh {
			t.Errorf("Size(%d, %d, %d) = %dx%d, want %dx%d", c.w, c.h, c.max, w, h, c.ww, c.wh)
		}
	}
}

func TestAdapter(t *testing.T) {
	a := NewAdapter()
	if a.Level().Name != "balanced" {
		t.Fatalf("starts at %s", a.Level().Name)
	}
	// a slow link: waiting most of the time - down a level per round, to the last
	for i := 0; i < 10; i++ {
		a.Observe(round, round*0.8)
	}
	if a.Level().Name != "minimal" {
		t.Fatalf("a slow link: %s", a.Level().Name)
	}
	// not a level up before upAfter of keeping up
	a.Observe(round, 0)
	if a.Level().Name != "minimal" {
		t.Fatalf("up too soon: %s", a.Level().Name)
	}
	// a fast link: up one level per upAfter
	ups := 0
	for i := 0; i < (int(upAfter/round)+1)*len(Levels); i++ {
		if a.Observe(round, 0) {
			ups++
		}
	}
	if a.Level().Name != "high" || ups != len(Levels)-1 {
		t.Fatalf("a fast link: %s after %d ups", a.Level().Name, ups)
	}
	// in between: stays
	if a.Observe(round, round*0.2) {
		t.Fatalf("moved on a link that keeps up most of the time")
	}
}

func TestValid(t *testing.T) {
	for _, p := range append(Presets(), "") {
		if err := Valid(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
	if Valid("ultra") == nil {
		t.Error("ultra is not a preset")
	}
}
