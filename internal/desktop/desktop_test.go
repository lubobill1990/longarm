package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// devBoxTags are the IMDS tags of a real Dev Box (ids shortened).
var devBoxTags = []Tag{
	{"ms.inv.v0.backedby.origin.sourcearmid.0", "/subscriptions/5e044d2b-6148-41d6-9da7-276cad95d203/resourceGroups/devcenter-2615-34/providers/Microsoft.DevCenter/devcenters/devcenter-sgimoplppxaka-dc"},
	{"ms.inv.v0.backedby.origin.tenantid", "72f988bf-86f1-41af-91ab-2d7cd011db47"},
	{"ms.inv.v0.backedby.resourceattributes", "companyname: Microsoft"},
	{"platformsettings.host_environment", "false"},
}

func TestParseTags(t *testing.T) {
	dc, ok := ParseTags(devBoxTags)
	if !ok || dc.Name != "devcenter-sgimoplppxaka-dc" || dc.Tenant != "72f988bf-86f1-41af-91ab-2d7cd011db47" {
		t.Fatalf("got %+v %v", dc, ok)
	}
	for name, tags := range map[string][]Tag{
		"none":           nil,
		"no tenant":      devBoxTags[:1],
		"no center":      devBoxTags[1:],
		"bad tenant":     {devBoxTags[0], {"ms.inv.v0.backedby.origin.tenantid", "x/../y"}},
		"other resource": {{"ms.inv.v0.backedby.origin.sourcearmid.0", "/subscriptions/x/providers/Microsoft.Compute/virtualMachines/vm"}, devBoxTags[1]},
		"bad name":       {{"ms.inv.v0.backedby.origin.sourcearmid.0", "/providers/Microsoft.DevCenter/devcenters/a.evil.com"}, devBoxTags[1]},
	} {
		if dc, ok := ParseTags(tags); ok {
			t.Errorf("%s: got %+v", name, dc)
		}
	}
}

func TestQueryIMDS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata") != "true" {
			http.Error(w, "no metadata header", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"name":"CPC_x","tagsList":[{"name":"ms.inv.v0.backedby.origin.sourcearmid.0","value":"/subscriptions/s/resourceGroups/g/providers/Microsoft.DevCenter/devcenters/dc1"},{"name":"ms.inv.v0.backedby.origin.tenantid","value":"72f988bf-86f1-41af-91ab-2d7cd011db47"}]}`))
	}))
	defer srv.Close()
	defer func(u string) { imdsURL = u }(imdsURL)
	imdsURL = srv.URL
	tags, ok := queryIMDS(context.Background())
	if !ok {
		t.Fatal("queryIMDS failed")
	}
	if dc, ok := ParseTags(tags); !ok || dc.Name != "dc1" {
		t.Fatalf("got %+v %v", dc, ok)
	}
	srv.Close()
	if _, ok := queryIMDS(context.Background()); ok {
		t.Fatal("queryIMDS succeeded without a server")
	}
}

// TestLastRemoteLogon: the newest logon / reconnect after Windows was
// installed; the image's own events (before it) don't count.
func TestLastRemoteLogon(t *testing.T) {
	installed := time.Date(2026, 10, 26, 6, 17, 7, 0, time.UTC)
	xml := `<Event><System><EventID>25</EventID><TimeCreated SystemTime='2026-10-29T05:24:11.1234567Z'/></System></Event>` +
		`<Event><System><EventID>21</EventID><TimeCreated SystemTime='2026-10-27T05:07:01.0000000Z'/></System></Event>` +
		`<Event><System><EventID>21</EventID><TimeCreated SystemTime='2026-10-25T10:22:55.0000000Z'/></System></Event>`
	if got := LastRemoteLogon(xml, installed); !got.Equal(time.Date(2026, 10, 29, 5, 24, 11, 123456700, time.UTC)) {
		t.Fatalf("got %v", got)
	}
	if got := LastRemoteLogon(`<Event><System><TimeCreated SystemTime='2026-10-25T10:22:55.0000000Z'/></System></Event>`, installed); !got.IsZero() {
		t.Fatalf("an image event counted: %v", got)
	}
}
