package tray

// The popup window's native background: what shows before the page paints
// (and around it while it loads). It matches the page's --bg
// (web/src/tray/tokens.css) for the effective theme, so a light-theme user
// sees no dark flash when the popup opens.

// RGB is a background colour.
type RGB struct{ R, G, B uint8 }

var (
	// PopupLight and PopupDark: tokens.css --bg, light and dark.
	PopupLight = RGB{0xfb, 0xfa, 0xf8}
	PopupDark  = RGB{0x1c, 0x1b, 0x1a}
)

// EffectiveTheme: "light" or "dark" for Settings > Appearance (tray.json
// "theme": "light", "dark", or "" = the system's) and whether the system
// uses a light app theme.
func EffectiveTheme(pref string, systemLight bool) string {
	switch pref {
	case "light", "dark":
		return pref
	}
	if systemLight {
		return "light"
	}
	return "dark"
}

// PopupBackground: the popup's background for the theme preference.
func PopupBackground(pref string, systemLight bool) RGB {
	if EffectiveTheme(pref, systemLight) == "light" {
		return PopupLight
	}
	return PopupDark
}

// CurrentPopupBackground: the background for the theme saved in dir's
// tray.json and the system's app theme now.
func CurrentPopupBackground(dir string) RGB {
	return PopupBackground(LoadPrefs(dir).Theme, SystemLightTheme())
}

// ThemeListener is a Desktop that follows Settings > Appearance (the
// native window's background).
type ThemeListener interface {
	ThemeChanged()
}
