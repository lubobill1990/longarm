package server

import (
	"bytes"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/workspace"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestSyncLFSUploadBounded: the uploaded LFS tar of a Workspace.Sync is cut
// off at the worker's limit (workspace.Store.MaxLFSUploadBytes) instead of
// filling the disk first.
func TestSyncLFSUploadBounded(t *testing.T) {
	e := newAuthzEnv(t)
	ws := e.srv.ws()
	ws.LFSLimits = workspace.LFSLimits{MaxObjectBytes: 1, MaxTotalBytes: 1, MaxObjects: 1}
	max := ws.MaxLFSUploadBytes()
	st, err := pb.NewWorkspaceClient(e.alice).Sync(cctx(t))
	if err != nil {
		t.Fatal(err)
	}
	st.Send(&pb.SyncChunk{Msg: &pb.SyncChunk_Header{Header: &pb.SyncHeader{Repo: "proj", Commit: "0000000000000000000000000000000000000000", StageOnly: true}}})
	chunk := bytes.Repeat([]byte{0}, 64<<10)
	for sent := int64(0); sent <= max+int64(len(chunk)); sent += int64(len(chunk)) {
		if err := st.Send(&pb.SyncChunk{Msg: &pb.SyncChunk_Data{Data: &pb.SyncData{Part: pb.SyncPart_SYNC_PART_LFS_TAR, Data: chunk}}}); err != nil {
			break // the server gave up
		}
	}
	_, err = st.CloseAndRecv()
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized LFS upload: %v", err)
	}
}
