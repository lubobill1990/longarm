package e2e

import (
	"encoding/xml"
	"os"
	"strings"
	"testing"
)

// TestMSIPropertiesOverridable (static, every platform): a property given
// on the msiexec command line must win over the value an earlier install
// remembered in the registry - an upgrade may change the hub, replace the
// CA pin or switch between join code and LOGIN. AppSearch overwrites the
// property it searches into, so HUB, CA_FINGERPRINT, LOGIN and LABELS must not
// carry a RegistrySearch themselves; their SAVED_* twin does, and a
// SetProperty applies it only when the property is empty.
func TestMSIPropertiesOverridable(t *testing.T) {
	b, err := os.ReadFile("../packaging/windows/longarm.wxs")
	if err != nil {
		t.Fatal(err)
	}
	type search struct {
		Name string `xml:"Name,attr"`
	}
	type property struct {
		ID     string   `xml:"Id,attr"`
		Search []search `xml:"RegistrySearch"`
	}
	type setProperty struct {
		ID        string `xml:"Id,attr"`
		Value     string `xml:"Value,attr"`
		Condition string `xml:"Condition,attr"`
	}
	var doc struct {
		Package struct {
			Properties []property    `xml:"Property"`
			Sets       []setProperty `xml:"SetProperty"`
		} `xml:"Package"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	props := map[string]property{}
	for _, p := range doc.Package.Properties {
		props[p.ID] = p
	}
	for _, name := range []string{"HUB", "CA_FINGERPRINT", "LOGIN", "LABELS"} {
		p, ok := props[name]
		if !ok {
			t.Errorf("no property %s", name)
			continue
		}
		if len(p.Search) != 0 {
			t.Errorf("%s searches the registry itself: AppSearch would override the command line", name)
		}
		if s := props["SAVED_"+name]; len(s.Search) != 1 {
			t.Errorf("SAVED_%s: no registry search", name)
		}
		found := false
		for _, s := range doc.Package.Sets {
			if s.ID == name && s.Value == "[SAVED_"+name+"]" {
				found = true
				if !strings.Contains(s.Condition, "NOT "+name+" ") {
					t.Errorf("SetProperty %s applies the saved value unconditionally: %q", name, s.Condition)
				}
			}
		}
		if !found {
			t.Errorf("no SetProperty %s = [SAVED_%s]", name, name)
		}
	}
}
