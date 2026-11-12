package hub

import (
	"net/http/httptest"
	"strings"
	"testing"

	"longarm/internal/notices"
)

// /licenses.txt: the front page's footer link, no sign-in.
func TestServeLicenses(t *testing.T) {
	rec := httptest.NewRecorder()
	serveLicenses(rec, httptest.NewRequest("GET", "/licenses.txt", nil))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || rec.Body.String() != notices.Text {
		t.Fatalf("%d %q %d bytes", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
}
