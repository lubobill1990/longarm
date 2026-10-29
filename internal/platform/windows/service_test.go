//go:build windows

package windows

import (
	"testing"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

// TestQueryServiceAnyAccount: QueryService works without elevation (the
// user-session longarmd and the tray ask whether LongarmUpdate exists): a
// service every Windows has is found running, a missing one is "not
// installed" - not an access-denied error, which made a user-session worker
// think the update service was absent.
func TestQueryServiceAnyAccount(t *testing.T) {
	st, err := QueryService("EventLog")
	if err != nil || st != ptypes.ServiceRunning {
		t.Fatalf("EventLog: %v, %v (want running)", st, err)
	}
	st, err = QueryService("LongarmNoSuchService-7f3a")
	if err != nil || st != ptypes.ServiceNotInstalled {
		t.Fatalf("missing service: %v, %v (want not installed)", st, err)
	}
}
