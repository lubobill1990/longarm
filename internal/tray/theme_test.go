package tray

import "testing"

func TestPopupBackground(t *testing.T) {
	for _, c := range []struct {
		pref        string
		systemLight bool
		want        RGB
	}{
		{"", true, PopupLight}, // System, Windows in light mode
		{"", false, PopupDark}, // System, dark mode
		{"light", false, PopupLight},
		{"dark", true, PopupDark},
		{"purple", true, PopupLight}, // unknown: the system's
	} {
		if got := PopupBackground(c.pref, c.systemLight); got != c.want {
			t.Errorf("PopupBackground(%q, %v) = %v, want %v", c.pref, c.systemLight, got, c.want)
		}
	}
	// the saved preference wins over the system's
	dir := t.TempDir()
	if err := SavePrefs(dir, Prefs{Theme: "dark"}); err != nil {
		t.Fatal(err)
	}
	if got := CurrentPopupBackground(dir); got != PopupDark {
		t.Fatalf("saved dark: %v", got)
	}
	SavePrefs(dir, Prefs{Theme: "light"})
	if got := CurrentPopupBackground(dir); got != PopupLight {
		t.Fatalf("saved light: %v", got)
	}
}
