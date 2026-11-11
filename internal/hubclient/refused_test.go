package hubclient

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCertRefused: the hub's refusals of this node's certificate (what the
// tray turns into "sign in again") against failures that are not.
func TestCertRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{status.Error(codes.Unavailable, `connection error: desc = "error reading server preface: remote error: tls: bad certificate"`), true},
		{status.Error(codes.Unauthenticated, "node win11 (abc) is revoked"), true},
		{status.Error(codes.PermissionDenied, "certificate 12 of worker/x is not known to the hub, and the name x has history"), true},
		{status.Error(codes.Unavailable, "connection refused"), false},
		{errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"), false}, // ours, not the hub's
		{status.Error(codes.PermissionDenied, "not your worker"), false},
		{nil, false},
	} {
		if got := CertRefused(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
	if !ReplacedGone(status.Error(codes.PermissionDenied, "replaces win11: no such enrolled node (removed, or its certificate revoked)")) {
		t.Error("replaced node gone not recognized")
	}
	if ReplacedGone(status.Error(codes.PermissionDenied, "replaces win11: an expired certificate that is not its latest")) {
		t.Error("another replace refusal taken for gone")
	}
}
