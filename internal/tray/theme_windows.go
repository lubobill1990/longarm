package tray

import "golang.org/x/sys/windows/registry"

// SystemLightTheme: Windows' app mode (Settings > Personalization >
// Colors), HKCU\...\Themes\Personalize AppsUseLightTheme; light when unset
// (Windows' default).
func SystemLightTheme() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return true
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err != nil || v != 0
}
