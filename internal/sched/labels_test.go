package sched

import (
	"maps"
	"testing"
)

func TestParseLabels(t *testing.T) {
	ok := []struct {
		args []string
		want map[string]string
	}{
		{nil, nil},
		{[]string{"region=eu"}, map[string]string{"region": "eu"}},
		{[]string{"region=eu,os=windows"}, map[string]string{"region": "eu", "os": "windows"}},
		{[]string{"region=eu, os=windows "}, map[string]string{"region": "eu", "os": "windows"}},
		{[]string{"region=eu", "os=windows", "region=eu"}, map[string]string{"region": "eu", "os": "windows"}},
		{[]string{"gpu=nvidia/a100", "zone=eu-west-1a", "v=1.2+x"}, map[string]string{"gpu": "nvidia/a100", "zone": "eu-west-1a", "v": "1.2+x"}},
	}
	for _, c := range ok {
		got, err := ParseLabels(c.args, false)
		if err != nil || !maps.Equal(got, c.want) {
			t.Errorf("ParseLabels(%q) = %v, %v; want %v", c.args, got, err, c.want)
		}
	}
	bad := [][]string{
		{"region"}, {"=eu"}, {"region=eu,"}, {",region=eu"}, {"region=eu,,os=w"}, {"region=eu,os"},
		{"region=eu=x"}, {"region=s g"}, {"region=eu", "region=us"}, {"re gion=eu"}, {"region="},
		{"-x=1"},
	}
	for _, b := range bad {
		if got, err := ParseLabels(b, false); err == nil {
			t.Errorf("ParseLabels(%q) accepted: %v", b, got)
		}
	}
	// "key=" deletes (longarm admin label).
	if got, err := ParseLabels([]string{"region=,os=linux"}, true); err != nil || got["region"] != "" || got["os"] != "linux" {
		t.Errorf("allowEmpty: %v %v", got, err)
	}
	if err := ValidateLabels(map[string]string{"region": "eu,os=windows"}, false); err == nil {
		t.Error("ValidateLabels accepted a comma value")
	}
}
