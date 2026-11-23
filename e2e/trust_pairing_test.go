package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"longarm/internal/hubclient"
	"longarm/internal/pki"
	"longarm/internal/trust"
	pb "longarm/proto/longarm/v1"
)

func automaticTrustHub(t *testing.T) *hubEnv {
	t.Helper()
	h := newHubEnv(t, "")
	h.mustLongarm(h.admin, "admin", "user", "add", "alice", "--no-code")
	h.startLongarmd()
	h.mustLongarm(h.client, "ls", "--json")
	h.mustLongarm(h.client, "ls", "--json")
	return h
}

func workerPairCommand(t *testing.T, h *hubEnv, name string) (string, []string) {
	t.Helper()
	r := h.longarm(h.client, "worker", "add", name)
	m := regexp.MustCompile(`(?m)^  longarmd (join .+)\r?$`).FindStringSubmatch(r.stderr)
	if r.code != 0 || m == nil {
		t.Fatalf("worker add did not print a join command: %+v", r)
	}
	args := strings.Fields(m[1])
	for i, arg := range args {
		if arg == "--pair" && i+1 < len(args) {
			secret := args[i+1]
			if _, err := pki.DerivePairing(secret); err != nil {
				t.Fatal(err)
			}
			if args[1] != h.addr() || args[2] != strings.TrimSpace(r.stdout) {
				t.Fatalf("join command does not match hub/code: %+v", r)
			}
			return secret, args
		}
	}
	t.Fatalf("worker add did not print --pair: %+v", r)
	return "", nil
}

func trustBundle(t *testing.T, dir string) (*pki.Bundle, *pki.TrustExt) {
	t.Helper()
	b, err := pki.LoadBundle(filepath.Join(dir, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pki.TrustOf(b.Leaf())
	if err != nil || tx == nil {
		t.Fatalf("certificate trust extension: %+v, %v", tx, err)
	}
	return b, tx
}

func checkAutomaticEndorsement(t *testing.T, dir, kind string, user pki.UserKeyLine) {
	t.Helper()
	b, tx := trustBundle(t, dir)
	e := tx.Endorsement
	if e == nil || !e.V2() || e.ID != b.Identity.ID || e.Name != b.Identity.Name ||
		e.Kind != kind || e.User != user.User || !bytes.Equal(e.UserKey, user.Key) ||
		!bytes.Equal(e.NodeKey, tx.NodeKey) {
		t.Fatalf("endorsement does not bind this node to alice's key: %+v (%v)", e, b.Identity)
	}
	if err := e.Verify(); err != nil {
		t.Fatal(err)
	}
}

func automaticUserKey(t *testing.T, dir string) pki.UserKeyLine {
	t.Helper()
	r := longarmWith(t, dir, nil, "trust", "show")
	if r.code != 0 {
		t.Fatalf("trust show: %+v", r)
	}
	for _, line := range strings.Split(r.stdout, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "longarm-user-key.v1.") {
			key, err := pki.ParseUserKeyLine(strings.TrimSpace(line))
			if err != nil {
				t.Fatal(err)
			}
			return key
		}
	}
	t.Fatal("trust show has no user key")
	return pki.UserKeyLine{}
}

func noWorkerTrustPolicy(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "trust", trust.PolicyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("automatic pairing created a worker enforcement policy: %v", err)
	}
}

func TestAutomaticTrustWorkerPairing(t *testing.T) {
	t.Parallel()
	h := automaticTrustHub(t)
	user := automaticUserKey(t, h.client)
	secret, args := workerPairCommand(t, h, "worker-1")
	w := h.newWorkerDir(t, "worker-1", "")
	if r := longarmdCmd(t, append(args, "--data-dir", w.dataDir)...); r.code != 0 ||
		!strings.Contains(r.stdout, "joined as worker worker-1") || strings.Contains(r.stdout, "warning:") {
		t.Fatalf("paired join: %+v", r)
	}
	pinned, err := trust.ReadPinnedUser(w.dataDir)
	if err != nil || pinned.String() != user.String() {
		t.Fatalf("pinned user differs from alice's key: %+v, %v", pinned, err)
	}
	noWorkerTrustPolicy(t, w.dataDir)
	if _, tx := trustBundle(t, w.dataDir); tx.Endorsement != nil {
		t.Fatal("worker was endorsed before alice's next session")
	}
	w.start(t, true)
	direct := fmt.Sprintf("worker-1@127.0.0.1:%d", w.port)
	r := h.longarm(h.client, "exec", direct, "--", bins.helper, "print", "1", "0", "0")
	if r.code != 0 || !strings.Contains(r.stdout, "line 1") || !strings.Contains(r.stderr, "endorsed your worker worker-1 (") {
		t.Fatalf("alice's exec and session endorsement: %+v", r)
	}
	waitFor(t, "automatic worker endorsement in its certificate", 30*time.Second, func() bool {
		_, tx := trustBundle(t, w.dataDir)
		return tx.Endorsement != nil
	})
	checkAutomaticEndorsement(t, w.dataDir, pki.KindWorker, user)
	noWorkerTrustPolicy(t, w.dataDir)
	pair, err := pki.DerivePairing(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trust.ReadPairing(h.client, pair.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed worker pairing retained: %v", err)
	}
	if r := h.longarm(h.client, "exec", direct, "--", bins.helper, "print", "1", "0", "0"); r.code != 0 || !strings.Contains(r.stdout, "line 1") {
		t.Fatalf("alice's exec after renewal: %+v", r)
	}
}

// pairingJoinProxy changes only the trust reply, leaving real enrollment to the hub.
type pairingJoinProxy struct {
	pb.UnimplementedEnrollServer
	upstream pb.EnrollClient
}

func (p *pairingJoinProxy) Join(ctx context.Context, req *pb.JoinRequest) (*pb.JoinResponse, error) {
	resp, err := p.upstream.Join(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Pairing == nil || len(resp.Pairing.UserTag) == 0 {
		return nil, status.Error(codes.Internal, "real hub returned no worker pairing")
	}
	resp.Pairing.UserTag[0] ^= 1
	return resp, nil
}

func TestAutomaticTrustWorkerKeySwap(t *testing.T) {
	t.Parallel()
	h := automaticTrustHub(t)
	_, args := workerPairCommand(t, h, "worker-1")
	ca, err := pki.LoadCA(filepath.Join(h.client, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := ca.Issue(pki.IssueOptions{Role: pki.RoleHub, Name: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(append(cert, ca.CertPEM...), key)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pki.LoadBundle(filepath.Join(h.admin, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := hubclient.Dial(h.addr(), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, NextProtos: []string{"h2"},
	})))
	pb.RegisterEnrollServer(server, &pairingJoinProxy{upstream: conn.Enroll()})
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		server.Stop()
		if err := <-done; err != nil {
			t.Errorf("pairing proxy: %v", err)
		}
	}()
	args[1] = listener.Addr().String()
	w := h.newWorkerDir(t, "worker-1", "")
	r := longarmdCmd(t, append(args, "--data-dir", w.dataDir)...)
	if r.code != 0 || !strings.Contains(r.stdout, "joined as worker worker-1") ||
		!strings.Contains(r.stdout, "automatic trust pairing failed:") {
		t.Fatalf("tampered response must warn but still enroll: %+v", r)
	}
	b, _ := trustBundle(t, w.dataDir)
	if b.Identity.Role != pki.RoleWorker || b.Identity.Name != "worker-1" || b.Identity.ID == "" {
		t.Fatalf("worker enrollment lost after pairing failure: %v", b.Identity)
	}
	cfg, err := hubclient.LoadFile(w.dataDir)
	if err != nil || cfg.Owner != "alice" {
		t.Fatalf("owned enrollment not saved: %+v, %v", cfg, err)
	}
	if _, err := trust.ReadPinnedUser(w.dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tampered user tag pinned a key: %v", err)
	}
	audit, err := os.ReadFile(filepath.Join(w.dataDir, "audit.log"))
	if err != nil || !strings.Contains(string(audit), `"method":"trust/pair-failed"`) {
		t.Fatalf("missing local pairing failure audit: %s, %v", audit, err)
	}
	noWorkerTrustPolicy(t, w.dataDir)
}

func TestAutomaticTrustOldStyleJoin(t *testing.T) {
	t.Parallel()
	h := automaticTrustHub(t)
	secret, args := workerPairCommand(t, h, "worker-1")
	b, err := pki.LoadBundle(filepath.Join(h.client, "tls"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := hubclient.Dial(h.addr(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, csr, err := pki.NewKeyAndCSR("worker-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := conn.Enroll().Join(ctx, &pb.JoinRequest{Code: args[2] + "#" + secret, CsrPem: csr}); status.Code(err) != codes.InvalidArgument ||
		!strings.Contains(status.Convert(err).Message(), "--pair") {
		t.Fatalf("hub accepted an unsplit local secret: %v", err)
	}
	// The rejected request must not consume the code; a legacy join still works.
	w := h.newWorkerDir(t, "worker-1", "")
	r := longarmdCmd(t, "join", h.addr(), args[2], "--ca-fingerprint", h.fp, "--data-dir", w.dataDir)
	if r.code != 0 || !strings.Contains(r.stdout, "joined as worker worker-1") ||
		strings.Contains(r.stdout+r.stderr, "automatic trust") {
		t.Fatalf("old-style join: %+v", r)
	}
	if _, err := trust.ReadPinnedUser(w.dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old-style join pinned a user: %v", err)
	}
	noWorkerTrustPolicy(t, w.dataDir)

	secret, args = workerPairCommand(t, h, "worker-2")
	w2 := h.newWorkerDir(t, "worker-2", "")
	r = longarmdCmd(t, "join", h.addr(), args[2]+"#"+secret, "--ca-fingerprint", h.fp, "--data-dir", w2.dataDir)
	if r.code != 0 || !strings.Contains(r.stdout, "joined as worker worker-2") || strings.Contains(r.stdout, "warning:") {
		t.Fatalf("CODE#SECRET join: %+v", r)
	}
	pinned, err := trust.ReadPinnedUser(w2.dataDir)
	if err != nil || pinned.String() != automaticUserKey(t, h.client).String() {
		t.Fatalf("CODE#SECRET did not pin alice: %+v, %v", pinned, err)
	}
	noWorkerTrustPolicy(t, w2.dataDir)
}

func automaticSecondDevice(t *testing.T, h *hubEnv) (string, string) {
	t.Helper()
	r := h.longarm(h.client, "panel-login")
	creds := regexp.MustCompile(`password (\S+)\nTOTP secret (\S+)`).FindStringSubmatch(r.stderr)
	if r.code != 0 || creds == nil {
		t.Fatalf("alice panel-login: %+v", r)
	}
	login := (&uiStack{hubEnv: h, user: "alice", pass: creds[1], secret: creds[2]}).login(t)
	browser := &panel{t: t, base: h.httpURL(), c: login.c}
	dir := filepath.Join(h.root, "alice-2")
	cmd := exec.Command(bins.longarm, "login", h.addr(), "--link", "--no-browser", "--name", "alice-2", "--ca-fingerprint", h.fp)
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+dir)
	p := startProc(t, cmd)
	_, code := p.code(t)
	if out := browser.decide(code, "approve", "client device"); !strings.Contains(out, "signed in as alice (client)") {
		t.Fatalf("approve alice's second device: %s", out)
	}
	if code := p.wait(t); code != 0 {
		t.Fatalf("second device login: %d\n%s", code, p.out.String())
	}
	m := regexp.MustCompile(`longarm device approve ([A-Z0-9-]+)`).FindStringSubmatch(p.out.String())
	if m == nil {
		t.Fatalf("second device did not offer automatic pairing:\n%s", p.out.String())
	}
	workers, err := os.ReadFile(filepath.Join(h.client, "workers.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workers.toml"), workers, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, m[1]
}

func TestAutomaticTrustDevicePairing(t *testing.T) {
	t.Parallel()
	h := automaticTrustHub(t)
	user := automaticUserKey(t, h.client)
	dir, secret := automaticSecondDevice(t, h)
	ts := trust.Store{Dir: filepath.Join(dir, "trust")}
	if _, _, err := ts.UserKey(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new device already has a user key: %v", err)
	}
	if _, tx := trustBundle(t, dir); tx.Endorsement != nil {
		t.Fatal("new device already endorsed")
	}
	r := h.longarm(h.client, "device", "approve", secret, "--yes")
	if r.code != 0 || !strings.Contains(r.stderr, "Trust device alice-2 (") || strings.Contains(r.stderr, "automatic trust:") {
		t.Fatalf("device approval: %+v", r)
	}
	r = h.longarm(dir, "ls", "--json")
	if r.code != 0 || !strings.Contains(r.stderr, "this device is now endorsed") {
		t.Fatalf("install device endorsement: %+v", r)
	}
	checkAutomaticEndorsement(t, dir, pki.KindDevice, user)
	if _, _, err := ts.UserKey(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("device approval transferred the secret user key: %v", err)
	}
	policy, err := ts.LoadPolicyFile()
	if err != nil || policy.RequireWorkers || policy.Users["alice"] != user.String() {
		t.Fatalf("paired device policy: %+v, %v", policy, err)
	}
}

func TestAutomaticTrustRecovery(t *testing.T) {
	t.Parallel()
	h := automaticTrustHub(t)
	user := automaticUserKey(t, h.client)
	code := strings.TrimSpace(h.mustLongarm(h.client, "trust", "show", "--recovery"))
	if !strings.HasPrefix(code, "LAR1-") {
		t.Fatalf("not a recovery code: %q", code)
	}
	dir, _ := automaticSecondDevice(t, h)
	r := h.longarm(dir, "trust", "recover", code)
	if r.code != 0 || !strings.Contains(r.stdout, "restored your user key and endorsed this device") {
		t.Fatalf("recover user key: %+v", r)
	}
	if got := automaticUserKey(t, dir); got.String() != user.String() {
		t.Fatal("recovered user key differs from the original trust show key line")
	}
	checkAutomaticEndorsement(t, dir, pki.KindDevice, user)
	policy, err := (trust.Store{Dir: filepath.Join(dir, "trust")}).LoadPolicyFile()
	if err != nil || policy.RequireWorkers {
		t.Fatalf("recovery enabled client enforcement: %+v, %v", policy, err)
	}
}
