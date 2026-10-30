package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/hub"
	"github.com/lubobill1990/longarm/internal/hub/store"
	"github.com/lubobill1990/longarm/internal/hubclient"
	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// TestRelayCertAlias: longarm-hub-relay-cert (az container exec next to the
// running hub, deploy.sh --relay) on a hub whose relay has history - the
// production failure after #45: the relay node was registered by an older
// certificate (no node ID), and a re-issued certificate without the node's
// ID was refused ("the name ... has history"). The alias now issues for that
// node (its ID in the certificate, the serial recorded against it): the
// relay subscribes, renews through the hub, and another upgrade works too.
func TestRelayCertAlias(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hub.toml")
	toml := fmt.Sprintf("data_dir = %q\nlisten = \"127.0.0.1:0\"\nrelay_udp = \"off\"\nhttp = \"127.0.0.1:0\"\n", filepath.ToSlash(dir))
	if err := os.WriteFile(cfgPath, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"LONGARM_HUB_DATA_DIR", "LONGARM_HUB_DB"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("LONGARM_HUB_CONFIG", cfgPath)
	t.Setenv("LONGARM_HUB_ADVERTISE", "myhub.eastasia.azurecontainer.io")
	ca, err := hub.InitCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := hub.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	h, err := hub.Open(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; h.Close() })
	for h.Addr("grpc") == "" {
		time.Sleep(10 * time.Millisecond)
	}

	subscribe := func(caPEM, certPEM, keyPEM []byte) error {
		t.Helper()
		b, err := pki.ParseBundle(caPEM, certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		st, err := pb.NewHubNodeClient(conn).RelaySync(sctx, &pb.RelaySyncRequest{Udp: "127.0.0.1:3478", Version: "test"})
		if err != nil {
			return err
		}
		_, err = st.Recv() // the first policy: subscribed
		return err
	}

	// the relay as an older hub image issued it (before node IDs): it
	// registers its node, and the name has history from now on
	oldCert, oldKey, err := ca.Issue(pki.IssueOptions{Role: pki.RoleRelay, Name: "myhub-relay", Hosts: []string{"myhub-relay.eastasia.azurecontainer.io"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := subscribe(ca.CertPEM, oldCert, oldKey); err != nil {
		t.Fatalf("the original relay: %v", err)
	}
	oldLeaf, _ := pki.ParseCertPEM(oldCert)
	nodeID := ""
	for _, n := range h.ListNodes(true) {
		if n.Name == "myhub-relay" {
			nodeID = n.Id
		}
	}
	if nodeID == "" {
		t.Fatal("no relay node")
	}
	// what v0.1.24's alias did: a certificate without the node's ID
	staleCert, staleKey, _ := ca.Issue(pki.IssueOptions{Role: pki.RoleRelay, Name: "myhub-relay"})
	if err := subscribe(ca.CertPEM, staleCert, staleKey); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "has history") {
		t.Fatalf("a certificate without the node's ID: %v", err)
	}

	relayCert := func() (caPEM, certPEM, keyPEM []byte) {
		t.Helper()
		args, err := aliasArgs("/usr/local/bin/longarm-hub-relay-cert")
		if err != nil || args == nil {
			t.Fatalf("alias: %v %v", args, err)
		}
		root := newRoot()
		root.SetArgs(args)
		out := captureStdout(t, func() {
			if err := root.Execute(); err != nil {
				t.Fatalf("longarm-hub-relay-cert: %v", err)
			}
		})
		return untarBundle(t, strings.TrimSpace(out))
	}
	for i := range 2 { // the upgrade, and the next one
		caPEM, certPEM, keyPEM := relayCert()
		leaf, err := pki.ParseCertPEM(certPEM)
		if err != nil {
			t.Fatal(err)
		}
		id, err := pki.IdentityFromCert(leaf)
		if err != nil {
			t.Fatal(err)
		}
		if id.ID != nodeID || id.Name != "myhub-relay" || id.Role != pki.RoleRelay ||
			!slices.Contains(leaf.DNSNames, "myhub-relay.eastasia.azurecontainer.io") {
			t.Fatalf("issue %d: identity %+v, DNS %v (node %s)", i, id, leaf.DNSNames, nodeID)
		}
		if got, _, _ := certRecord(t, cfg, pki.Serial(leaf)); got != nodeID {
			t.Fatalf("issue %d: the serial is recorded for %q, not the node %s", i, got, nodeID)
		}
		if err := subscribe(caPEM, certPEM, keyPEM); err != nil {
			t.Fatalf("issue %d: the relay with the new certificate: %v", i, err)
		}
		// it renews through the hub (relay certificates carry no user)
		b, _ := pki.ParseBundle(caPEM, certPEM, keyPEM)
		conn, err := grpc.NewClient(h.Addr("grpc"), grpc.WithTransportCredentials(credentials.NewTLS(hubclient.TLSConfig(b))))
		if err != nil {
			t.Fatal(err)
		}
		_, csr, _ := pki.NewKeyAndCSR("", nil)
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		rr, err := pb.NewHubNodeClient(conn).Renew(rctx, &pb.RenewRequest{CsrPem: csr})
		rcancel()
		conn.Close()
		if err != nil {
			t.Fatalf("issue %d: renewal: %v", i, err)
		}
		rl, _ := pki.ParseCertPEM(rr.CertPem)
		if rid, _ := pki.IdentityFromCert(rl); rid.ID != nodeID || !slices.Contains(rl.DNSNames, "myhub-relay.eastasia.azurecontainer.io") {
			t.Fatalf("issue %d: renewed %+v %v", i, rid, rl.DNSNames)
		}
	}
	// the original certificate is still the node's
	if err := subscribe(ca.CertPEM, oldCert, oldKey); err != nil {
		t.Fatalf("the original relay certificate %s: %v", pki.Serial(oldLeaf), err)
	}
	relays := 0
	for _, n := range h.ListNodes(true) {
		if n.Role == string(pki.RoleRelay) {
			relays++
		}
	}
	if relays != 1 {
		t.Fatalf("%d relay nodes, want 1", relays)
	}
}

func certRecord(t *testing.T, cfg *hub.Config, serial string) (string, bool, error) {
	t.Helper()
	st, err := store.Open(cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	return st.CertRecord(serial)
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	var buf bytes.Buffer
	copied := make(chan struct{})
	go func() { io.Copy(&buf, r); close(copied) }()
	defer func() { os.Stdout = old }() // f failed (t.Fatal)
	f()
	os.Stdout = old
	w.Close()
	<-copied
	return buf.String()
}

func untarBundle(t *testing.T, b64 string) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hd, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files[hd.Name], _ = io.ReadAll(tr)
	}
	return files[pki.CACertFile], files[pki.NodeCertFile], files[pki.NodeKeyFile]
}
