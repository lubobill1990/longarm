// Messages to the Wails shell when longarm-tray's popup runs in its webview
// (the page comes from longarm-tray's loopback server, so the Wails runtime JS
// is not loaded; the native message channel still is).
export function wailsMessage(msg: string): boolean {
  const w = window as unknown as {
    chrome?: { webview?: { postMessage(m: string): void } }
    webkit?: { messageHandlers?: { external?: { postMessage(m: string): void } } }
  }
  if (w.chrome?.webview?.postMessage) {
    w.chrome.webview.postMessage(msg)
    return true
  }
  if (w.webkit?.messageHandlers?.external?.postMessage) {
    w.webkit.messageHandlers.external.postMessage(msg)
    return true
  }
  return false
}
