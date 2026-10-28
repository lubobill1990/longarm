import { useCallback, useEffect, useState } from 'react'
import { traySettingsApi } from '../api/local'

/** Settings > Appearance: follow the system (Windows' app mode), or light / dark. */
export type ThemePref = 'system' | 'light' | 'dark'

// the last choice, for the first paint (the tray's settings are the truth)
const KEY = 'longarm.tray.theme'
const media = () => window.matchMedia?.('(prefers-color-scheme: dark)')

export function parseTheme(v: unknown): ThemePref {
  return v === 'light' || v === 'dark' ? v : 'system'
}

export function loadTheme(): ThemePref {
  try {
    return parseTheme(localStorage.getItem(KEY))
  } catch {
    return 'system'
  }
}

/** The theme a preference resolves to now. */
export function resolveTheme(pref: ThemePref, systemDark = !!media()?.matches): 'light' | 'dark' {
  return pref === 'dark' || (pref === 'system' && systemDark) ? 'dark' : 'light'
}

/** Sets <html data-theme> (tokens.css) to the resolved theme. */
export function applyTheme(pref: ThemePref): void {
  document.documentElement.dataset.theme = resolveTheme(pref)
}

/**
 * The popup's theme: applied at once, re-applied when the system's
 * changes (pref "system"), saved in the tray's settings (tray.json) and
 * read back from them at start.
 */
export function useTheme(): [ThemePref, (p: ThemePref) => Promise<void>] {
  const [pref, setPref] = useState<ThemePref>(loadTheme)
  useEffect(() => {
    applyTheme(pref)
    try {
      if (pref === 'system') localStorage.removeItem(KEY)
      else localStorage.setItem(KEY, pref)
    } catch {
      // no storage: the tray's settings still have it
    }
    if (pref !== 'system') return
    const mq = media()
    const on = () => applyTheme('system')
    mq?.addEventListener?.('change', on)
    return () => mq?.removeEventListener?.('change', on)
  }, [pref])
  useEffect(() => {
    traySettingsApi
      .get()
      .then((s) => setPref(parseTheme(s.theme)))
      .catch(() => {}) // an older tray: the local choice
  }, [])
  const set = useCallback(async (p: ThemePref) => {
    setPref(p)
    await traySettingsApi.set({ theme: p })
  }, [])
  return [pref, set]
}
