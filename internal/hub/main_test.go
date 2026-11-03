package hub

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Hubs under test never read the public repository's releases
	// (ghreleases.go); the tests that want GitHub's set their own.
	os.Setenv(EnvReleasesGitHub, "off")
	os.Exit(m.Run())
}
