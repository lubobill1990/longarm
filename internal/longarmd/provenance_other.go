//go:build !windows

package longarmd

// ProvenanceLocation is where the provenance values live (in memory: the
// installer's data dirs are Windows-only).
const ProvenanceLocation = "memory"

func newProvenance() ProvenanceStore { return &memProvenance{} }

func trustedSystemPath(string, ...string) error { return nil }
