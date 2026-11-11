package tray

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/lubobill1990/longarm/internal/localapi"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// refused reports whether the hub refuses the certificate at certPath: the
// daemon using it says so (HubLink.refused), and the file is not newer than
// that refusal (a sign-in since wrote a new one the daemon has yet to use).
func (s *Server) refused(certPath string, link func() *pb.HubLink) bool {
	if link == nil {
		return false
	}
	l := link()
	if l == nil || !l.GetRefused() {
		return false
	}
	fi, err := os.Stat(certPath)
	return err == nil && (l.GetSince() == nil || !fi.ModTime().After(l.GetSince().AsTime()))
}

// localWorkerLink: longarmd's hub link (nil: not running, not answering
// within a second - account() runs on every info poll).
func (s *Server) localWorkerLink() *pb.HubLink {
	if s.opts.LongarmdDataDir == "" {
		return nil
	}
	var l *pb.HubLink
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.localWorker(ctx, func(ctx context.Context, c pb.LocalWorkerClient) error {
		st, err := c.Status(ctx, &pb.LocalStatusRequest{})
		if err == nil {
			l = st.GetHub()
		}
		return err
	})
	return l
}

// localClientLink: the client daemon's hub link (nil: not running).
func (s *Server) localClientLink() *pb.HubLink {
	cc, err := localapi.DialFile(filepath.Join(s.opts.LongarmConfigDir, "daemon.addr"), "longarm daemon")
	if err != nil {
		return nil
	}
	defer cc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	st, err := pb.NewLocalClientClient(cc).Status(ctx, &pb.LocalStatusRequest{})
	if err != nil {
		return nil
	}
	return st.GetHub()
}
