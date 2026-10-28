//go:build production

package main

// releaseBuild: built with the "production" tag (releases: .goreleaser.yaml,
// the MSI job): Wails turns the WebView2 devtools and its debug context
// menu off.
const releaseBuild = true
