// Package localapi is the transport of the local control API (DESIGN
// §11.3) served by longarmd and `longarm daemon`: one local channel only the current
// user can reach (Windows named pipe with an owner-only DACL; elsewhere a
// 0600 unix socket in a private 0700 directory), speaking both gRPC
// (HTTP/2 without TLS: the channel itself is the access control) and an
// HTTP/1.1 JSON/SSE gateway for UIs. The channel's address is written to a
// file next to the owner's data, so clients find it.
package localapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/lubobill1990/longarm/internal/platform"
)

// Listen opens the local channel for kind ("longarmd" / "longarm") and dir (the data
// or config dir; one channel per dir) and records its address in addrFile.
func Listen(kind, dir, addrFile string) (net.Listener, string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256([]byte(strings.ToLower(abs)))
	ln, addr, err := platform.ListenLocal(kind + "-" + hex.EncodeToString(sum[:6]))
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(addrFile, []byte(addr+"\n"), 0o600); err != nil {
		ln.Close()
		return nil, "", err
	}
	return ln, addr, nil
}

// ReadAddr reads an address file written by Listen.
func ReadAddr(addrFile string) (string, error) {
	b, err := os.ReadFile(addrFile)
	if err != nil {
		return "", err
	}
	a := strings.TrimSpace(string(b))
	if a == "" {
		return "", errors.New("empty address file " + addrFile)
	}
	return a, nil
}

// Serve serves g (gRPC over h2c) and gateway (everything else) on ln until
// ctx is done.
func Serve(ctx context.Context, ln net.Listener, g *grpc.Server, gateway http.Handler) error {
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				g.ServeHTTP(w, r)
				return
			}
			gateway.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	srv.Protocols = p
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			srv.Close()
		case <-done:
		}
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Dial connects a gRPC client to a local channel address.
func Dial(addr string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///local",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return platform.DialLocal(ctx, addr) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// NotRunningError: no address file, so the process isn't running;
// errors.Is(err, os.ErrNotExist) holds (callers add how to start it).
type NotRunningError struct{ What, AddrFile string }

func (e *NotRunningError) Error() string {
	return fmt.Sprintf("%s is not running (no %s)", e.What, e.AddrFile)
}

func (e *NotRunningError) Is(target error) bool { return target == os.ErrNotExist }

// DialFile dials the address recorded in addrFile.
func DialFile(addrFile, what string) (*grpc.ClientConn, error) {
	a, err := ReadAddr(addrFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &NotRunningError{What: what, AddrFile: addrFile}
		}
		return nil, err
	}
	return Dial(a)
}

// HTTPClient returns an HTTP client whose connections go to the local
// channel (for the JSON gateway; any host name works).
func HTTPClient(addr string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return platform.DialLocal(ctx, addr) },
	}}
}

// ---------------------------------------------------------------- gateway helpers

var mo = protojson.MarshalOptions{UseProtoNames: true}

// WriteProto writes m as JSON (or an error as {"error": ...}).
func WriteProto(w http.ResponseWriter, m proto.Message, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "{\"error\":%q}\n", err.Error())
		return
	}
	b, _ := mo.Marshal(m)
	w.Write(b)
}

// ReadProto decodes a JSON request body into m (empty body = zero m).
func ReadProto(r *http.Request, m proto.Message) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	return protojson.Unmarshal(b, m)
}

// SSE streams messages from next (nil = end) as server-sent events of kind.
func SSE(w http.ResponseWriter, r *http.Request, kind string, next func(ctx context.Context) (proto.Message, error)) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, ": ok\n\n")
	fl.Flush()
	for {
		m, err := next(r.Context())
		if err != nil || m == nil {
			return
		}
		b, _ := mo.Marshal(m)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b); err != nil {
			return
		}
		fl.Flush()
	}
}

// SSEStream streams the messages run sends as server-sent events of kind;
// an error from run ends the stream with an "error" event ({"error": ...}).
func SSEStream(w http.ResponseWriter, r *http.Request, kind string, run func(ctx context.Context, send func(proto.Message) error) error) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, ": ok\n\n")
	fl.Flush()
	err := run(r.Context(), func(m proto.Message) error {
		b, err := mo.Marshal(m)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b); err != nil {
			return err
		}
		fl.Flush()
		return nil
	})
	if err != nil && r.Context().Err() == nil {
		b, _ := json.Marshal(map[string]string{"error": status.Convert(err).Message()})
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", b)
	} else {
		fmt.Fprint(w, "event: end\ndata: {}\n\n")
	}
	fl.Flush()
}
