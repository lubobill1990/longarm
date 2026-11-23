//go:build darwin

package darwin

import (
	"encoding/xml"
	"reflect"
	"strings"
	"testing"

	"longarm/internal/platform/ptypes"
)

func TestPlist(t *testing.T) {
	for _, user := range []string{"", "alice"} {
		t.Run("user="+user, func(t *testing.T) {
			cfg := ptypes.ServiceConfig{
				Name: "longarmd", Exe: "/usr/local/bin/longarmd",
				Args: []string{"run", "--data-dir", `/tmp/longarm & <jobs> "test"`}, User: user,
			}
			plist := Plist(cfg)
			var got struct {
				Dict struct {
					Keys    []string   `xml:"key"`
					Strings []string   `xml:"string"`
					Array   []string   `xml:"array>string"`
					True    []struct{} `xml:"true"`
				} `xml:"dict"`
			}
			if err := xml.Unmarshal([]byte(plist), &got); err != nil {
				t.Fatal(err)
			}
			wantKeys := []string{"Label", "ProgramArguments"}
			wantStrings := []string{"com.longarm.longarmd"}
			if user != "" {
				wantKeys = append(wantKeys, "UserName")
				wantStrings = append(wantStrings, user)
			}
			wantKeys = append(wantKeys, "ProcessType", "RunAtLoad", "KeepAlive", "AbandonProcessGroup")
			wantStrings = append(wantStrings, "Interactive")
			if !reflect.DeepEqual(got.Dict.Keys, wantKeys) || !reflect.DeepEqual(got.Dict.Strings, wantStrings) ||
				!reflect.DeepEqual(got.Dict.Array, append([]string{cfg.Exe}, cfg.Args...)) || len(got.Dict.True) != 3 {
				t.Fatalf("unexpected plist:\n%s", plist)
			}
			if !strings.Contains(plist, "<key>ProcessType</key><string>Interactive</string>") ||
				!strings.Contains(plist, "<key>AbandonProcessGroup</key><true/>") {
				t.Fatalf("missing launchd settings:\n%s", plist)
			}
		})
	}
}
