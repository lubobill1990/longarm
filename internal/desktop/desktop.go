// Package desktop finds out how this machine's desktop is reached
// (InfoResponse.desktop, `longarm desktop`): a Microsoft Dev Box (Windows
// App through its Dev Center), a Windows 365 Cloud PC, plain Remote Desktop
// or none.
//
// Whether the machine is a Dev Box comes from the Azure Instance Metadata
// Service: Dev Box VMs carry the tag
// "ms.inv.v0.backedby.origin.sourcearmid.0" naming their Dev Center
// (".../providers/Microsoft.DevCenter/devcenters/<name>") and
// "ms.inv.v0.backedby.origin.tenantid". It is asked once (the answer does
// not change while longarmd runs); Remote Desktop's state is read on every
// call (the owner may turn it on or off).
package desktop

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Kinds of DesktopInfo.kind.
const (
	KindDevBox  = "devbox"
	KindCloudPC = "cloudpc"
	KindRDP     = "rdp"
)

// imdsURL is the Azure Instance Metadata Service's compute endpoint
// (a variable for tests).
var imdsURL = "http://169.254.169.254/metadata/instance/compute?api-version=2021-02-01"

// DevCenter identifies a Dev Box's Dev Center.
type DevCenter struct {
	Name   string
	Tenant string
}

// devCenterRE matches the Dev Center part of an ARM resource id.
var devCenterRE = regexp.MustCompile(`(?i)/providers/Microsoft\.DevCenter/devcenters/([A-Za-z0-9-]{3,63})(?:/|$)`)

// tenantRE: an Entra tenant id.
var tenantRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ParseTags finds the Dev Center in IMDS compute tags (tagsList); ok is
// false on machines that are not Dev Boxes.
func ParseTags(tags []Tag) (DevCenter, bool) {
	var dc DevCenter
	for _, t := range tags {
		switch {
		case strings.HasPrefix(t.Name, "ms.inv.v0.backedby.origin.sourcearmid."):
			if m := devCenterRE.FindStringSubmatch(t.Value); m != nil {
				dc.Name = strings.ToLower(m[1])
			}
		case t.Name == "ms.inv.v0.backedby.origin.tenantid":
			if tenantRE.MatchString(t.Value) {
				dc.Tenant = strings.ToLower(t.Value)
			}
		}
	}
	return dc, dc.Name != "" && dc.Tenant != ""
}

// Tag is one entry of the IMDS compute tagsList.
type Tag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// queryIMDS asks the metadata service for this VM's tags; ok is false off
// Azure (no metadata service) and on errors.
func queryIMDS(ctx context.Context) ([]Tag, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsURL, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Metadata", "true")
	// Never through a proxy: the metadata service is link-local.
	c := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var out struct {
		TagsList []Tag `json:"tagsList"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, false
	}
	return out.TagsList, true
}

// Detector answers InfoResponse.desktop; the zero value is ready to use.
type Detector struct {
	once sync.Once
	done chan struct{}
	dc   DevCenter
	isDB bool

	mu      sync.Mutex
	factsAt time.Time
	facts   devBoxFacts
}

// devBoxFacts are what tells a Dev Box apart from the user's others
// (DesktopInfo.os_installed, last_remote_logon).
type devBoxFacts struct {
	installed, lastLogon time.Time
}

// factsTTL: the last remote logon is read from the event log at most this often.
const factsTTL = time.Minute

func (d *Detector) devBoxFacts() devBoxFacts {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.factsAt.IsZero() || time.Since(d.factsAt) > factsTTL {
		d.facts, d.factsAt = readDevBoxFacts(), time.Now()
	}
	return d.facts
}

// Start asks the metadata service in the background (Info does not wait
// for it longer than a moment).
func (d *Detector) Start(ctx context.Context) {
	d.once.Do(func() {
		d.done = make(chan struct{})
		go func() {
			defer close(d.done)
			if !mayBeCloudVM() {
				return
			}
			if tags, ok := queryIMDS(ctx); ok {
				d.dc, d.isDB = ParseTags(tags)
			}
		}()
	})
}

// Info describes the desktop now.
func (d *Detector) Info(ctx context.Context) *pb.DesktopInfo {
	d.Start(context.WithoutCancel(ctx))
	select {
	case <-d.done:
	case <-time.After(4 * time.Second):
	case <-ctx.Done():
	}
	select {
	case <-d.done:
		if d.isDB {
			out := &pb.DesktopInfo{Kind: KindDevBox, DevCenter: d.dc.Name, DevCenterTenant: d.dc.Tenant}
			f := d.devBoxFacts()
			if !f.installed.IsZero() {
				out.OsInstalled = timestamppb.New(f.installed)
			}
			if !f.lastLogon.IsZero() {
				out.LastRemoteLogon = timestamppb.New(f.lastLogon)
			}
			return out
		}
	default:
	}
	return localInfo()
}

// remoteLogonTimes finds the TimeCreated of remote desktop logons and
// reconnects in wevtutil's XML output (newest first).
var systemTimeRE = regexp.MustCompile(`SystemTime=['"]([0-9T:.\-]+Z?)['"]`)

// LastRemoteLogon picks the newest event time after installed from
// wevtutil's XML (events before it come with the image: the machine it was
// captured from).
func LastRemoteLogon(xml string, installed time.Time) time.Time {
	var last time.Time
	for _, m := range systemTimeRE.FindAllStringSubmatch(xml, -1) {
		t, err := time.Parse(time.RFC3339Nano, m[1])
		if err != nil {
			continue
		}
		if t.After(installed) && t.After(last) {
			last = t
		}
	}
	return last
}
