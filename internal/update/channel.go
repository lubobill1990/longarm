package update

import "strings"

// Release channels, from the most to the least tested
// (docs/ci-cd.md "Release channels"):
//
//   - stable: a vX.Y.Z tag; what everybody gets by default
//   - preview: a vX.Y.Z-<pre> tag (release candidates)
//   - dev: every push to main, X.Y.Z-dev.<n>
//
// A machine on a channel takes the newest release of that channel or of a
// more tested one: preview machines get stable releases too, dev machines
// everything.
const (
	ChannelStable  = "stable"
	ChannelPreview = "preview"
	ChannelDev     = "dev"
)

// Channels in order (the tray's choices).
var Channels = []string{ChannelStable, ChannelPreview, ChannelDev}

// NormalizeChannel maps a channel name to one of Channels: "" is stable,
// "beta" (the prerelease channel before preview existed) is preview; ok is
// false for names that are none of them (other custom channels).
func NormalizeChannel(c string) (string, bool) {
	switch c = strings.ToLower(strings.TrimSpace(c)); c {
	case "", ChannelStable:
		return ChannelStable, true
	case ChannelPreview, "beta":
		return ChannelPreview, true
	case ChannelDev:
		return ChannelDev, true
	}
	return c, false
}

// channelRank: 0 stable, 1 preview, 2 dev; -1 for other channels.
func channelRank(c string) int {
	n, ok := NormalizeChannel(c)
	if !ok {
		return -1
	}
	switch n {
	case ChannelPreview:
		return 1
	case ChannelDev:
		return 2
	}
	return 0
}

// ChannelIncludes reports whether a machine on channel subscriber takes a
// release published to channel release: the same channel, or a more tested
// one of Channels. Other (custom) channels only take themselves.
func ChannelIncludes(subscriber, release string) bool {
	s, r := channelRank(subscriber), channelRank(release)
	if s < 0 || r < 0 {
		return strings.EqualFold(strings.TrimSpace(subscriber), strings.TrimSpace(release))
	}
	return r <= s
}
