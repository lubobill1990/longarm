package hub

import (
	"slices"
	"sort"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestReleaseRoleTable: the CI's release certificate may publish releases
// and start/inspect/abort rollouts, and call no other hub method - checked
// against every RPC of every longarm service, so a method added later
// without a table entry stays denied.
func TestReleaseRoleTable(t *testing.T) {
	want := []string{
		pb.HubAdmin_AbortRollout_FullMethodName,
		pb.HubAdmin_GetRollout_FullMethodName,
		pb.HubAdmin_ListReleases_FullMethodName,
		pb.HubAdmin_PublishRelease_FullMethodName,
		pb.HubAdmin_PushRelease_FullMethodName,
	}
	var got []string
	n := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("longarm.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := 0; i < fd.Services().Len(); i++ {
			sd := fd.Services().Get(i)
			for j := 0; j < sd.Methods().Len(); j++ {
				m := "/" + string(sd.FullName()) + "/" + string(sd.Methods().Get(j).Name())
				n++
				if slices.Contains(hubMethodRoles[m], pki.RoleRelease) {
					got = append(got, m)
				}
			}
		}
		return true
	})
	sort.Strings(got)
	if n < 50 {
		t.Fatalf("only %d methods found in the registry", n)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("release role may call %v, want exactly %v", got, want)
	}
	// Admins keep every release method.
	for _, m := range want {
		if !slices.Contains(hubMethodRoles[m], pki.RoleAdmin) {
			t.Errorf("admin lost %s", m)
		}
	}
}
