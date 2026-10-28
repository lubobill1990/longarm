package hub

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/update"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// Release store and rollouts (DESIGN §7.2).
//
// `longarm-hub release add <dir> --version V [--channel C]` copies the release
// files into <data>/releases/<V>/ and records their sha256 (and their
// minisign signature, <file>.minisig, when present):
//   - binaries  <tool>-<os>-<arch>[.exe]  (longarm, longarmd, longarm-tray, longarm-hub, longarm-relay)
//   - packages  longarm-<os>-<arch>.{msi,deb,rpm,pkg,tar.gz,zip}
//
// GET /releases/<V>/<file> serves them to install scripts. Workers fetch
// updates through gRPC (Fleet.DownloadRelease) and verify the signatures
// against the key built into them.
//
// `longarm admin release push V --canary N --wait D [--batch B --batch-wait D]`
// starts a rollout: N canary workers are told to run V; once they all run it
// and stay online for --wait, the others follow in batches of B, with
// --batch-wait between batches. A worker that rolls back (its new version
// failed its health check) halts the rollout. One rollout exists at a time;
// it is persisted in the meta table.

var (
	binaryFileRE  = update.BinaryFileRE
	packageFileRE = update.PackageFileRE
	versionRE     = update.VersionRE
	channelRE     = update.ChannelRE
)

// servableFile reports whether name may be served from a release dir.
func servableFile(name string) bool {
	base := strings.TrimSuffix(name, update.SigExt)
	return binaryFileRE.MatchString(base) || packageFileRE.MatchString(base)
}

// AddRelease imports the release files in dir as version on channel. With
// publicKey set, every signature present must verify and update binaries
// (longarmd, longarm, longarm-tray) must be signed.
func AddRelease(dataDir string, st *store.Store, dir, version, channel, publicKey string) ([]store.Release, error) {
	if !versionRE.MatchString(version) {
		return nil, fmt.Errorf("invalid version %q", version)
	}
	channel = cmp.Or(channel, "stable")
	if !channelRE.MatchString(channel) {
		return nil, fmt.Errorf("invalid channel %q", channel)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	// 1. Check every file before anything is stored, so a refused file
	// can't leave a half-imported version behind.
	type item struct {
		r   store.Release
		src string
	}
	var items []item
	for _, e := range ents {
		name := e.Name()
		r := store.Release{Version: version, File: name, Channel: channel, AddedAt: time.Now()}
		if m := binaryFileRE.FindStringSubmatch(name); m != nil {
			r.OS, r.Arch, r.Kind = m[2], m[3], "binary"
		} else if m := packageFileRE.FindStringSubmatch(name); m != nil {
			r.OS, r.Arch, r.Kind = m[1], m[2], "package"
		} else {
			continue
		}
		if e.IsDir() {
			continue
		}
		src := filepath.Join(dir, name)
		if r.SHA256, r.Size, err = hashFile(src); err != nil {
			return nil, err
		}
		if sig, err := os.ReadFile(src + update.SigExt); err == nil {
			r.Sig = sig
		}
		updatable := r.Kind == "binary" && isUpdateBinary(name)
		if publicKey != "" {
			switch {
			case r.Sig != nil:
				data, err := os.ReadFile(src)
				if err != nil {
					return nil, err
				}
				if err := update.Verify(publicKey, data, r.Sig, name, version); err != nil {
					return nil, fmt.Errorf("refusing %s: %w", name, err)
				}
			case updatable:
				return nil, fmt.Errorf("refusing %s: no %s signature (sign with `longarm admin release sign` or `longarm-hub release sign`)", name, update.SigExt)
			}
		}
		items = append(items, item{r, src})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no release files in %s (want names like longarmd-linux-amd64, longarm-windows-amd64.exe, longarm-windows-amd64.msi)", dir)
	}
	// 2. Store them.
	dst := filepath.Join(dataDir, ReleasesDir, version)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, err
	}
	var added []store.Release
	for _, it := range items {
		r, name := it.r, it.r.File
		if err := copyFile(it.src, filepath.Join(dst, name), 0o755); err != nil {
			return nil, err
		}
		if r.Sig != nil {
			if err := os.WriteFile(filepath.Join(dst, name+update.SigExt), r.Sig, 0o644); err != nil {
				return nil, err
			}
		}
		if err := st.AddRelease(r); err != nil {
			return nil, err
		}
		added = append(added, r)
	}
	if channel == "stable" {
		return added, st.SetMeta("latest_release", version)
	}
	return added, nil
}

func isUpdateBinary(name string) bool { return update.IsUpdateBinary(name) }

// SignRelease writes <file>.minisig for every release file in dir.
func SignRelease(dir, version, keyFile, password string) ([]string, error) {
	return update.SignDir(dir, version, keyFile, password)
}

// releaseFiles returns the files of version (all kinds).
func (h *Hub) releaseFiles(version string) ([]store.Release, error) {
	rels, err := h.store.Releases()
	if err != nil {
		return nil, err
	}
	var out []store.Release
	for _, r := range rels {
		if r.Version == version {
			out = append(out, r)
		}
	}
	return out, nil
}

func releaseFilePB(r store.Release) *pb.ReleaseFile {
	return &pb.ReleaseFile{Name: r.File, Sha256: r.SHA256, Size: r.Size, Signature: r.Sig, Os: r.OS, Arch: r.Arch}
}

// ---------------------------------------------------------------- rollouts

const rolloutMeta = "release_rollout"

// Rollout states.
const (
	rolloutCanary  = "canary"
	rolloutRolling = "rolling"
	rolloutDone    = "done"
	rolloutHalted  = "halted"
	rolloutAborted = "aborted"
)

func (h *Hub) loadRollout() {
	v, err := h.store.Meta(rolloutMeta)
	if err != nil || v == "" {
		return
	}
	r := &pb.Rollout{}
	if protojson.Unmarshal([]byte(v), r) == nil {
		h.rollout = r
	}
}

func (h *Hub) saveRolloutLocked() {
	if h.rollout == nil {
		return
	}
	b, err := protojson.Marshal(h.rollout)
	if err == nil {
		err = h.store.SetMeta(rolloutMeta, string(b))
	}
	if err != nil {
		h.log.Warn("save rollout", "err", err)
	}
}

func channelOf(n *node) string { return cmp.Or(n.rec.Channel, "stable") }

// updateFailedLocked reports whether n's last update to version failed. An
// "unhealthy" report counts too: the watchdog is about to roll back, and if
// it can't, the node must not pass as a healthy canary.
func updateFailedLocked(n *node, version string) (bool, string) {
	if len(n.rec.Update) == 0 {
		return false, ""
	}
	us := &pb.UpdateStatus{}
	if protojson.Unmarshal(n.rec.Update, us) != nil || us.To != version {
		return false, ""
	}
	switch us.State {
	case update.PhaseRolledBack, update.PhaseFailed:
		return true, us.Error
	case update.PhaseUnhealthy: // the watchdog's wording for the same failure
		return true, "health check failed: " + us.Error
	}
	return false, ""
}

// targetLocked is the version n should run ("" = stay).
func (h *Hub) targetLocked(n *node) string {
	r := h.rollout
	if r == nil || n.rec.Role != "worker" || n.rec.Admin || channelOf(n) != r.Channel {
		return ""
	}
	name := n.rec.Name
	if slices.Contains(r.Failed, name) {
		return ""
	}
	if failed, _ := updateFailedLocked(n, r.Version); failed {
		return ""
	}
	var ok bool
	switch r.State {
	case rolloutDone:
		ok = true
	case rolloutCanary, rolloutRolling:
		ok = slices.Contains(r.Targeted, name)
	case rolloutHalted, rolloutAborted:
		ok = slices.Contains(r.Updated, name) // keep, don't flip back and forth
	}
	if !ok {
		return ""
	}
	if update.Compare(n.rec.Version, r.Version) > 0 && !r.AllowDowngrade {
		return ""
	}
	return r.Version
}

func (h *Hub) nodeUpdatedLocked(n *node, version string) bool {
	return n.sess != nil && n.rec.Version == version
}

// hintLocked tells a connected node that its target changed.
func (h *Hub) hintLocked(n *node) {
	if t := h.targetLocked(n); t != "" && n.rec.Version != t {
		h.sendLocked(n, &pb.HubCommand{Msg: &pb.HubCommand_Update{Update: &pb.UpdateHint{Version: t}}})
	}
}

// pushRelease starts a rollout.
func (h *Hub) pushRelease(req *pb.PushReleaseRequest, by string) (*pb.Rollout, error) {
	files, err := h.releaseFiles(req.Version)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, status.Errorf(codes.NotFound, "no release %s (longarm-hub release add)", req.Version)
	}
	signed := false
	for _, f := range files {
		if strings.HasPrefix(f.File, "longarmd-") && f.Kind == "binary" && f.Sig != nil {
			signed = true
		}
	}
	if !signed {
		return nil, status.Errorf(codes.FailedPrecondition, "release %s has no signed longarmd binary; workers would refuse it", req.Version)
	}
	channel := files[0].Channel
	h.mu.Lock()
	defer h.mu.Unlock()
	var cands []*node
	var cantUpdate []string
	for _, n := range h.nodes {
		if n.rec.Role != "worker" || n.rec.State != stateApproved || channelOf(n) != channel {
			continue
		}
		if len(req.Nodes) > 0 && !slices.Contains(req.Nodes, n.rec.Name) {
			continue
		}
		if n.rec.Admin {
			continue // admin workers update with their MSI only, never by a rollout
		}
		if !n.rec.UpdateEnabled {
			// No update key in its build, or a package-managed install:
			// it would never update and stall the rollout.
			cantUpdate = append(cantUpdate, n.rec.Name)
			continue
		}
		cands = append(cands, n)
	}
	sort.Strings(cantUpdate)
	if len(cands) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "no approved worker of channel %s can self-update (not capable: %v)", channel, cantUpdate)
	}
	// Canary preference: online, update-capable, fewest active jobs.
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if (a.sess != nil) != (b.sess != nil) {
			return a.sess != nil
		}
		if a.rec.UpdateEnabled != b.rec.UpdateEnabled {
			return a.rec.UpdateEnabled
		}
		ja, _ := h.loadLocked(a.rec.Name)
		jb, _ := h.loadLocked(b.rec.Name)
		if ja != jb {
			return ja < jb
		}
		return a.rec.Name < b.rec.Name
	})
	old := h.rollout
	r := &pb.Rollout{Version: req.Version, Channel: channel, StartedAt: timestamppb.Now(),
		Wait: req.Wait, Batch: req.Batch, BatchWait: req.BatchWait, AllowDowngrade: req.AllowDowngrade}
	if r.Wait == nil {
		r.Wait = durationpb.New(30 * time.Minute)
	}
	if r.BatchWait == nil {
		r.BatchWait = durationpb.New(5 * time.Minute)
	}
	versions := map[string]int{}
	for _, n := range cands {
		versions[n.rec.Version]++
	}
	for v, c := range versions {
		if c > versions[r.Previous] || (c == versions[r.Previous] && v > r.Previous) {
			r.Previous = v
		}
	}
	canary := int(max(req.Canary, 0))
	canary = min(canary, len(cands))
	for i, n := range cands {
		if i < canary {
			r.Canary = append(r.Canary, n.rec.Name)
		} else {
			r.Waiting = append(r.Waiting, n.rec.Name)
		}
	}
	r.Targeted = slices.Clone(r.Canary)
	r.State = rolloutCanary
	if canary == 0 {
		r.State = rolloutRolling
		h.rollout = r
		h.nextBatchLocked()
	}
	r.Message = fmt.Sprintf("pushed by %s", by)
	if len(cantUpdate) > 0 {
		r.Message += fmt.Sprintf("; skipped (can't self-update: no key or package-managed): %s", strings.Join(cantUpdate, ","))
	}
	if old != nil && (old.State == rolloutCanary || old.State == rolloutRolling) {
		h.log.Info("release rollout replaced", "old", old.Version, "new", r.Version)
	}
	h.rollout = r
	h.saveRolloutLocked()
	h.log.Info("release rollout started", "version", r.Version, "channel", channel, "canary", r.Canary, "nodes", len(cands), "by", by)
	for _, name := range r.Targeted {
		if n := h.nodes[name]; n != nil {
			h.hintLocked(n)
		}
	}
	h.bus.publish(&pb.FleetEvent{Kind: "release", Message: fmt.Sprintf("rollout of %s started (%s)", r.Version, r.State)})
	return h.rolloutPBLocked(), nil
}

// nextBatchLocked moves the next batch from waiting to targeted.
func (h *Hub) nextBatchLocked() {
	r := h.rollout
	n := len(r.Waiting)
	if r.Batch > 0 {
		n = min(n, int(r.Batch))
	}
	batch := r.Waiting[:n]
	r.Targeted = append(r.Targeted, batch...)
	r.Waiting = slices.Clone(r.Waiting[n:])
	r.PhaseSince = nil
	for _, name := range batch {
		if nd := h.nodes[name]; nd != nil {
			h.hintLocked(nd)
		}
	}
	h.log.Info("release rollout: next batch", "version", r.Version, "nodes", batch)
}

// advanceReleaseLocked drives the rollout (called every second and on
// node changes).
func (h *Hub) advanceReleaseLocked() {
	r := h.rollout
	if r == nil || (r.State != rolloutCanary && r.State != rolloutRolling) {
		return
	}
	changed := false
	var updated []string
	for _, name := range r.Targeted {
		n := h.nodes[name]
		if n == nil {
			continue
		}
		if failed, why := updateFailedLocked(n, r.Version); failed && !slices.Contains(r.Failed, name) {
			r.Failed = append(r.Failed, name)
			r.State, r.Message = rolloutHalted, fmt.Sprintf("%s rolled back: %s", name, why)
			changed = true
			h.log.Warn("release rollout halted", "version", r.Version, "node", name, "err", why)
		}
		if h.nodeUpdatedLocked(n, r.Version) {
			updated = append(updated, name)
		}
	}
	for _, name := range r.Targeted {
		if n := h.nodes[name]; n != nil && n.sess == nil && n.rec.Version == r.Version && !slices.Contains(updated, name) {
			updated = append(updated, name) // updated earlier, offline right now
		}
	}
	sort.Strings(updated)
	if !slices.Equal(updated, r.Updated) {
		r.Updated, changed = updated, true
	}
	now := time.Now()
	allOnlineUpdated := func(names []string) bool {
		for _, name := range names {
			n := h.nodes[name]
			if n == nil || slices.Contains(r.Failed, name) {
				continue
			}
			if r.State == rolloutCanary && !h.nodeUpdatedLocked(n, r.Version) {
				return false // canaries must be online and updated
			}
			if n.sess != nil && !h.nodeUpdatedLocked(n, r.Version) {
				return false
			}
		}
		return true
	}
	switch r.State {
	case rolloutCanary:
		if !allOnlineUpdated(r.Canary) {
			if r.PhaseSince != nil {
				r.PhaseSince, changed = nil, true // a canary went away: the wait starts over
			}
			break
		}
		if r.PhaseSince == nil {
			r.PhaseSince, changed = timestamppb.New(now), true
			r.Message = fmt.Sprintf("canary %s healthy; waiting %s", strings.Join(r.Canary, ","), r.Wait.AsDuration())
		}
		if now.Sub(r.PhaseSince.AsTime()) >= r.Wait.AsDuration() {
			r.State, changed = rolloutRolling, true
			if len(r.Waiting) > 0 {
				h.nextBatchLocked()
			}
			r.Message = "canary passed; rolling"
		}
	case rolloutRolling:
		if !allOnlineUpdated(r.Targeted) {
			break
		}
		if len(r.Waiting) == 0 {
			r.State, r.Message, changed = rolloutDone, "all workers targeted and updated", true
			h.log.Info("release rollout done", "version", r.Version)
			break
		}
		if r.PhaseSince == nil {
			r.PhaseSince, changed = timestamppb.New(now), true
		}
		if now.Sub(r.PhaseSince.AsTime()) >= r.BatchWait.AsDuration() {
			h.nextBatchLocked()
			changed = true
		}
	}
	if changed {
		h.saveRolloutLocked()
		h.bus.publish(&pb.FleetEvent{Kind: "release", Message: fmt.Sprintf("rollout of %s: %s (%s)", r.Version, r.State, r.Message)})
	}
}

func (h *Hub) rolloutPBLocked() *pb.Rollout {
	if h.rollout == nil {
		return &pb.Rollout{}
	}
	return proto.Clone(h.rollout).(*pb.Rollout)
}

// ---------------------------------------------------------------- RPCs

func (s *fleetServer) CheckUpdate(ctx context.Context, req *pb.UpdateCheckRequest) (*pb.UpdateCheckResponse, error) {
	h := s.h
	c := callerFrom(ctx)
	h.mu.Lock()
	n := h.nodes[c.id.Name]
	if n == nil {
		h.mu.Unlock()
		return nil, status.Error(codes.NotFound, "unknown node")
	}
	if req.Channel != "" && channelRE.MatchString(req.Channel) {
		n.rec.Channel = req.Channel
	}
	if req.Version != "" {
		n.rec.Version = req.Version
	}
	target := h.targetLocked(n)
	state := ""
	if h.rollout != nil {
		state = h.rollout.Version + " " + h.rollout.State
	}
	h.mu.Unlock()
	resp := &pb.UpdateCheckResponse{Rollout: state}
	if target == "" || target == req.Version {
		return resp, nil
	}
	files, err := h.releaseFiles(target)
	if err != nil {
		return nil, err
	}
	resp.Version = target
	for _, f := range files {
		if f.Kind == "binary" && f.OS == req.Os && f.Arch == req.Arch && isUpdateBinary(f.File) {
			resp.Files = append(resp.Files, releaseFilePB(f))
		}
	}
	return resp, nil
}

func (s *fleetServer) DownloadRelease(req *pb.ReleaseFileRequest, stream pb.Fleet_DownloadReleaseServer) error {
	if !versionRE.MatchString(req.Version) || !servableFile(req.Name) {
		return status.Error(codes.InvalidArgument, "bad release file")
	}
	f, err := os.Open(filepath.Join(s.h.cfg.DataDir, ReleasesDir, req.Version, req.Name))
	if err != nil {
		return status.Errorf(codes.NotFound, "%s/%s: not found", req.Version, req.Name)
	}
	defer f.Close()
	if req.Offset > 0 {
		if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
			return err
		}
	}
	buf := make([]byte, 1<<20)
	off := req.Offset
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pb.FileChunk{Offset: off, Data: buf[:n]}); serr != nil {
				return serr
			}
			off += int64(n)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *hubAdminServer) ListReleases(context.Context, *pb.ListReleasesRequest) (*pb.ListReleasesResponse, error) {
	rels, err := s.h.store.Releases()
	if err != nil {
		return nil, err
	}
	byVersion := map[string]*pb.ReleaseVersion{}
	var order []string
	for _, r := range rels {
		v := byVersion[r.Version]
		if v == nil {
			v = &pb.ReleaseVersion{Version: r.Version, Channel: r.Channel, AddedAt: timestamppb.New(r.AddedAt), Signed: true}
			byVersion[r.Version] = v
			order = append(order, r.Version)
		}
		f := releaseFilePB(r)
		f.Signature = nil
		v.Files = append(v.Files, f)
		if isUpdateBinary(r.File) && r.Sig == nil {
			v.Signed = false
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return update.Compare(order[i], order[j]) > 0 })
	resp := &pb.ListReleasesResponse{}
	for _, v := range order {
		resp.Releases = append(resp.Releases, byVersion[v])
	}
	s.h.mu.Lock()
	resp.Rollout = s.h.rolloutPBLocked()
	s.h.mu.Unlock()
	return resp, nil
}

func (s *hubAdminServer) PushRelease(ctx context.Context, req *pb.PushReleaseRequest) (*pb.Rollout, error) {
	if !versionRE.MatchString(req.Version) {
		return nil, status.Error(codes.InvalidArgument, "bad version")
	}
	if req.AllowDowngrade && callerFrom(ctx).id.Role == pki.RoleRelease {
		// A stolen CI certificate must not be able to move the fleet back
		// to an older (signed, maybe vulnerable) version.
		return nil, status.Error(codes.PermissionDenied, "the release role may not downgrade workers (admin only)")
	}
	return s.h.pushRelease(req, callerFrom(ctx).id.Name)
}

func (s *hubAdminServer) GetRollout(context.Context, *pb.GetRolloutRequest) (*pb.Rollout, error) {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	return s.h.rolloutPBLocked(), nil
}

func (s *hubAdminServer) AbortRollout(ctx context.Context, _ *pb.GetRolloutRequest) (*pb.Rollout, error) {
	h := s.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.rollout == nil {
		return nil, status.Error(codes.NotFound, "no rollout")
	}
	if h.rollout.State == rolloutCanary || h.rollout.State == rolloutRolling {
		h.rollout.State = rolloutAborted
		h.rollout.Message = "aborted by " + callerFrom(ctx).id.Name
		h.saveRolloutLocked()
		h.bus.publish(&pb.FleetEvent{Kind: "release", Message: "rollout of " + h.rollout.Version + " aborted"})
	}
	return h.rolloutPBLocked(), nil
}

// Upload limits of PublishRelease.
var (
	maxUploadFiles = 64
	maxUploadBytes = int64(4 << 30)
)

// PublishRelease imports release files uploaded by an admin (longarm admin
// release publish), who signed them on their own machine: like
// `longarm-hub release add`, with the same signature checks.
func (s *hubAdminServer) PublishRelease(st pb.HubAdmin_PublishReleaseServer) error {
	first, err := st.Recv()
	if err != nil {
		return err
	}
	hdr := first.GetHeader()
	if hdr == nil {
		return status.Error(codes.InvalidArgument, "the first message must be the header")
	}
	if !versionRE.MatchString(hdr.Version) {
		return status.Errorf(codes.InvalidArgument, "invalid version %q", hdr.Version)
	}
	if hdr.Channel != "" && !channelRE.MatchString(hdr.Channel) {
		return status.Errorf(codes.InvalidArgument, "invalid channel %q", hdr.Channel)
	}
	if hdr.Replace && callerFrom(st.Context()).id.Role == pki.RoleRelease {
		return status.Error(codes.PermissionDenied, "the release role may not replace a published version (admin only)")
	}
	if !hdr.Replace {
		existing, err := s.h.releaseFiles(hdr.Version)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if len(existing) > 0 {
			return status.Errorf(codes.AlreadyExists, "version %s exists on the hub (--replace overwrites it; workers may be downloading it)", hdr.Version)
		}
	}
	root := filepath.Join(s.h.cfg.DataDir, ReleasesDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	stage, err := os.MkdirTemp(root, ".upload-")
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	defer os.RemoveAll(stage)
	var (
		cur   *os.File
		name  string
		total int64
		done  = map[string]bool{}
	)
	defer func() {
		if cur != nil {
			cur.Close()
		}
	}()
	for {
		m, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		c := m.GetChunk()
		if c == nil {
			return status.Error(codes.InvalidArgument, "expected a file chunk")
		}
		if cur == nil || c.Name != name {
			if cur != nil {
				return status.Errorf(codes.InvalidArgument, "%s: incomplete (no last chunk)", name)
			}
			if !update.IsReleaseFile(c.Name) {
				return status.Errorf(codes.InvalidArgument, "%q is not a release file name", c.Name)
			}
			if done[c.Name] {
				return status.Errorf(codes.InvalidArgument, "%s sent twice", c.Name)
			}
			if len(done) >= maxUploadFiles {
				return status.Errorf(codes.InvalidArgument, "more than %d files", maxUploadFiles)
			}
			name = c.Name
			if cur, err = os.OpenFile(filepath.Join(stage, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644); err != nil {
				return status.Error(codes.Internal, err.Error())
			}
		}
		if total += int64(len(c.Data)); total > maxUploadBytes {
			return status.Errorf(codes.ResourceExhausted, "upload larger than %d bytes", maxUploadBytes)
		}
		if _, err := cur.Write(c.Data); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		if c.Last {
			if err := cur.Close(); err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			cur = nil
			done[name] = true
			if len(c.Signature) > 0 {
				if len(c.Signature) > 4096 {
					return status.Errorf(codes.InvalidArgument, "%s: signature too large", name)
				}
				if err := os.WriteFile(filepath.Join(stage, name+update.SigExt), c.Signature, 0o644); err != nil {
					return status.Error(codes.Internal, err.Error())
				}
			}
		}
	}
	if cur != nil {
		return status.Errorf(codes.InvalidArgument, "%s: incomplete (no last chunk)", name)
	}
	if len(done) == 0 {
		return status.Error(codes.InvalidArgument, "no files")
	}
	who := by(st.Context())
	rels, err := AddRelease(s.h.cfg.DataDir, s.h.store, stage, hdr.Version, hdr.Channel, s.h.cfg.Releases.PublicKey)
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	v := &pb.ReleaseVersion{Version: hdr.Version, Channel: cmp.Or(hdr.Channel, "stable"), AddedAt: timestamppb.Now(), Signed: true}
	var names []string
	for _, r := range rels {
		f := releaseFilePB(r)
		f.Signature = nil
		v.Files = append(v.Files, f)
		names = append(names, r.File)
		if isUpdateBinary(r.File) && r.Sig == nil {
			v.Signed = false
		}
	}
	s.h.log.Info("release published", "version", hdr.Version, "channel", v.Channel, "files", len(rels), "signed", v.Signed, "by", who)
	s.h.auditHub(who, "HubAdmin.PublishRelease", map[string]any{"version": hdr.Version, "channel": v.Channel, "files": names, "signed": v.Signed})
	return st.SendAndClose(v)
}
