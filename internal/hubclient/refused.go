package hubclient

import (
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReplacedGone reports whether a StartLink failed because an enrollment it
// was to replace is no longer on the hub (removed, its certificate
// revoked: internal/hub/linkreplace.go) - nothing to replace any more.
func ReplacedGone(err error) bool {
	s, ok := status.FromError(err)
	return ok && s.Code() == codes.PermissionDenied && strings.Contains(s.Message(), "no such enrolled node")
}

// CertRefused reports whether the hub refused this node's own certificate
// (revoked, removed, unknown, disabled): signing in again is the way back,
// retrying is not. A TLS alert the hub sent ("remote error: tls: ...")
// is its refusal; our own failure to verify the hub is not.
func CertRefused(err error) bool {
	if err == nil {
		return false
	}
	if s, ok := status.FromError(err); ok {
		switch {
		case s.Code() == codes.Unauthenticated:
			return true
		case s.Code() == codes.PermissionDenied && (strings.Contains(s.Message(), "is not known to the hub") || strings.Contains(s.Message(), "is disabled")):
			return true
		}
	}
	msg := err.Error()
	for _, alert := range []string{"remote error: tls: bad certificate", "remote error: tls: revoked certificate",
		"remote error: tls: certificate required", "remote error: tls: unknown certificate authority", "remote error: tls: expired certificate"} {
		if strings.Contains(msg, alert) {
			return true
		}
	}
	return false
}
