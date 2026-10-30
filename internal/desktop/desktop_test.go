package desktop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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
