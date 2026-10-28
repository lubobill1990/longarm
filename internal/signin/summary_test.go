package signin

import "testing"

// TestSummary: the one line the tray, longarm login and longarmd login show
// names exactly what the machine became.
func TestSummary(t *testing.T) {
	for _, c := range []struct {
		r    Result
		want string
	}{
		{Result{Hub: "h:7443", User: "alice@contoso.com", Device: &Enrolled{Name: "alice-box"}, Worker: &Enrolled{Name: "box"},
			AdminWorker: &Enrolled{Name: "box-admin"}, WorkerKind: "user session"},
			"Signed in as alice@contoso.com: client device alice-box, worker box (user session), admin worker box-admin (runs as SYSTEM, yours only)"},
		{Result{Hub: "h:7443", User: "alice", Device: &Enrolled{Name: "alice-box"}}, "Signed in as alice: client device alice-box"},
		{Result{Hub: "h:7443", User: "alice", Worker: &Enrolled{Name: "box"}, WorkerKind: "service"}, "Signed in as alice: worker box (service)"},
		{Result{Hub: "h:7443", Worker: &Enrolled{Name: "box"}}, "Signed in: worker box"},
		{Result{Hub: "h:7443"}, "Signed in at h:7443"},
	} {
		if got := c.r.Summary(); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
}
