// Package skills carries the agent skills that ship with Longarm (embedded in
// the longarm and longarm-tray binaries, so every installer has them and they
// change with each release). internal/agentskill installs them into coding
// agents' user-global skills directories on request.
package skills

import "embed"

// FS holds longarm/SKILL.md and its reference files.
//
//go:embed longarm
var FS embed.FS
