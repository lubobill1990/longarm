// Package tgrpc connects internal/telemetry to gRPC: interceptors that
// propagate the W3C trace context (metadata "traceparent") from a client
// to the worker and the hub and record requests and dependencies; the
// conversion to the Telemetry service's messages; and the Pusher that
// hands a node's queue to longarm-hub.
package tgrpc

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/lubobill1990/longarm/internal/pki"
	"github.com/lubobill1990/longarm/internal/telemetry"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// MethodName shortens "/longarm.v1.Jobs/Submit" to "Jobs/Submit".
func MethodName(full string) string {
	full = strings.TrimPrefix(full, "/")
	if i := strings.LastIndexByte(full, '.'); i >= 0 && i < strings.IndexByte(full+"/", '/') {
		full = full[i+1:]
	}
	return full
}

// ServerFault reports whether a status code means the server failed (as
// opposed to refusing or not finding something).
func ServerFault(c codes.Code) bool {
	switch c {
	case codes.Unknown, codes.Internal, codes.Unavailable, codes.DataLoss, codes.Unimplemented, codes.DeadlineExceeded:
		return true
	}
	return false
}

// PeerRole is the certificate role of the caller ("" = none).
func PeerRole(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.PeerCertificates) == 0 {
		return ""
	}
	id, err := pki.IdentityFromCert(ti.State.PeerCertificates[0])
	if err != nil {
		return ""
	}
	return string(id.Role)
}

// skip: the telemetry push itself is not telemetry.
func skip(method string) bool { return method == pb.Telemetry_Push_FullMethodName }

// serverSpan derives the request's span from incoming metadata.
func serverSpan(ctx context.Context) telemetry.Span {
	var remote telemetry.Span
	ok := false
	if md, has := metadata.FromIncomingContext(ctx); has {
		if v := md.Get(telemetry.TraceparentKey); len(v) > 0 {
			remote, ok = telemetry.ParseTraceparent(v[0])
		}
	}
	return telemetry.ServerSpan(remote, ok)
}

func recordServer(c *telemetry.Client, ctx context.Context, method string, sp telemetry.Span, start time.Time, err error, stream bool) {
	if !c.Enabled() {
		return
	}
	code := status.Code(err)
	role := PeerRole(ctx)
	c.Request(ctx, telemetry.Request{Name: MethodName(method), Span: sp, Duration: time.Since(start), Code: code.String(),
		Success: !ServerFault(code), Stream: stream, Props: map[string]string{"caller_role": role}})
	if code == codes.PermissionDenied || code == codes.Unauthenticated {
		c.Count("rpc.denied", map[string]string{"method": MethodName(method), "grpc_code": code.String(), "caller_role": role}, 1)
	}
}

// UnaryServer starts an operation span per call (linked to the caller's
// traceparent) and records the request. Put it first in the chain: it
// also sees calls the authorization refuses.
func UnaryServer(tel func() *telemetry.Client) grpc.UnaryServerInterceptor {
	tel = orDefault(tel)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		if skip(info.FullMethod) {
			return h(ctx, req)
		}
		sp := serverSpan(ctx)
		ctx = telemetry.WithSpan(ctx, sp)
		start := time.Now()
		resp, err := h(ctx, req)
		recordServer(tel(), ctx, info.FullMethod, sp, start, err, false)
		return resp, err
	}
}

type spanStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *spanStream) Context() context.Context { return s.ctx }

// StreamServer is UnaryServer for streams (sent individually only when
// they fail: a stream lives as long as its client wants).
func StreamServer(tel func() *telemetry.Client) grpc.StreamServerInterceptor {
	tel = orDefault(tel)
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) error {
		if skip(info.FullMethod) {
			return h(srv, ss)
		}
		sp := serverSpan(ss.Context())
		ctx := telemetry.WithSpan(ss.Context(), sp)
		start := time.Now()
		err := h(srv, &spanStream{ServerStream: ss, ctx: ctx})
		recordServer(tel(), ctx, info.FullMethod, sp, start, err, true)
		return err
	}
}

// outgoing adds traceparent for a child span of ctx's span.
func outgoing(ctx context.Context) (context.Context, telemetry.Span, bool) {
	sp, ok := telemetry.SpanFrom(ctx)
	if !ok {
		return ctx, telemetry.Span{}, false
	}
	child := sp.Child()
	return metadata.AppendToOutgoingContext(ctx, telemetry.TraceparentKey, child.Traceparent()), child, true
}

// UnaryClient sends the trace context and records the call as a
// dependency on a target of kind target ("worker", "hub").
func UnaryClient(target string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if skip(method) {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		ctx, child, ok := outgoing(ctx)
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		if ok {
			code := status.Code(err)
			telemetry.RecordDependency(ctx, telemetry.Dependency{Type: "grpc", Target: target, Name: MethodName(method), Span: child,
				Duration: time.Since(start), Code: code.String(), Success: !ServerFault(code)})
		}
		return err
	}
}

// StreamClient sends the trace context on streams; it records a
// dependency only when the stream could not be opened.
func StreamClient(target string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		if skip(method) {
			return streamer(ctx, desc, cc, method, opts...)
		}
		ctx, child, ok := outgoing(ctx)
		start := time.Now()
		cs, err := streamer(ctx, desc, cc, method, opts...)
		if ok && err != nil {
			code := status.Code(err)
			telemetry.RecordDependency(ctx, telemetry.Dependency{Type: "grpc", Target: target, Name: MethodName(method), Span: child,
				Duration: time.Since(start), Code: code.String(), Success: !ServerFault(code), Stream: true})
		}
		return cs, err
	}
}

// DialOptions are the client interceptors for a target kind.
func DialOptions(target string) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(UnaryClient(target)),
		grpc.WithChainStreamInterceptor(StreamClient(target)),
	}
}

// ---------------------------------------------------------------- conversion

// ToPB converts an item.
func ToPB(it telemetry.Item) *pb.TelemetryItem {
	out := &pb.TelemetryItem{
		Kind: it.Kind, Time: timestamppb.New(it.Time), Name: it.Name, Message: it.Message, Severity: it.Severity,
		Props: it.Props, Measurements: it.Measurements, OperationId: it.OperationID, ParentId: it.ParentID, Id: it.ID,
		DurationMs: it.DurationMS, Success: it.Success, ResultCode: it.ResultCode, Type: it.Type, Target: it.Target,
		Value: it.Value, Count: it.Count, Min: it.Min, Max: it.Max,
		Context: &pb.TelemetryContext{Component: it.Context.Component, Version: it.Context.Version, Os: it.Context.OS,
			Arch: it.Context.Arch, Session: it.Context.Session, Fleet: it.Context.Fleet, Node: it.Context.Node,
			User: it.Context.User, Role: it.Context.Role},
	}
	for _, f := range it.Stack {
		out.Stack = append(out.Stack, &pb.TelemetryFrame{Function: f.Function, File: f.File, Line: int32(f.Line)})
	}
	return out
}

// FromPB converts an item back.
func FromPB(p *pb.TelemetryItem) telemetry.Item {
	it := telemetry.Item{
		Kind: p.Kind, Name: p.Name, Message: p.Message, Severity: p.Severity, Props: p.Props, Measurements: p.Measurements,
		OperationID: p.OperationId, ParentID: p.ParentId, ID: p.Id, DurationMS: p.DurationMs, Success: p.Success,
		ResultCode: p.ResultCode, Type: p.Type, Target: p.Target, Value: p.Value, Count: p.Count, Min: p.Min, Max: p.Max,
	}
	if p.Time != nil {
		it.Time = p.Time.AsTime()
	}
	if c := p.Context; c != nil {
		it.Context = telemetry.Context{Component: c.Component, Version: c.Version, OS: c.Os, Arch: c.Arch, Session: c.Session,
			Fleet: c.Fleet, Node: c.Node, User: c.User, Role: c.Role}
	}
	for _, f := range p.Stack {
		it.Stack = append(it.Stack, telemetry.Frame{Function: f.Function, File: f.File, Line: int(f.Line)})
	}
	return it
}

// CADER reads the CA certificate (DER) of a TLS bundle dir (ca.crt).
func CADER(tlsDir string) []byte {
	b, err := os.ReadFile(filepath.Join(tlsDir, pki.CACertFile))
	if err != nil {
		return nil
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil
	}
	if _, err := x509.ParseCertificate(blk.Bytes); err != nil {
		return nil
	}
	return blk.Bytes
}

// orDefault is tel, or the default client's getter.
func orDefault(tel func() *telemetry.Client) func() *telemetry.Client {
	if tel == nil {
		return telemetry.Default
	}
	return tel
}
