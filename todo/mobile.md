# The phone app (docs/mobile.md)

## The desktop: clipboard, sound, displays, direct touch

The phone viewer has no clipboard sync, no sound, no display switch (the
stream starts on display 0; `Started` reports how many there are) and only
the trackpad touch mode - a "direct" mode (tap where you touch) suits
tablets. Start from internal/mobile/desktop.go (the stream's other
messages are dropped there) and the two viewers (DesktopActivity.java,
DesktopViewController.swift).

## Modifier keys and shortcuts in the desktop viewer

The bar has Esc, Win and Ctrl+Alt+Del; there is no Ctrl / Alt / Shift
latch (the terminal's key bar has one, web/src/mobile/KeyBar.tsx) nor
Tab / function keys on screen, so Ctrl+C, Alt+Tab and the like can't be
typed without a hardware keyboard.

## Lock and notifications

Face ID / fingerprint before the app opens (the device certificate is a
key to the user's machines), the key in the Keychain / Keystore instead of
the app's files, and notifications (a session ended, an agent waits for
input) through the hub (APNs / FCM). docs/mobile.md phase 3.

## Android releases

TestFlight ships the iOS app from main; Android has no signed release
build or store listing yet: the APK is a debug build (mobile.yml's
android job).
