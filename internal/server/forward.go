package server

import (
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lubobill1990/longarm/internal/pki"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// SSHHandler serves one ssh connection (the built-in sshd); id is the
// authenticated mTLS identity of the caller.
type SSHHandler interface {
	HandleProxyConn(c net.Conn, id pki.Identity)
}

type forwardServer struct {
	pb.UnimplementedForwardServer
	s *Server
}

// forwardTarget normalizes "3131" to "127.0.0.1:3131".
func forwardTarget(t string) (string, error) {
	if p, err := strconv.Atoi(t); err == nil {
		if p <= 0 || p > 65535 {
			return "", errors.New("bad port")
		}
		return net.JoinHostPort("127.0.0.1", t), nil
	}
	host, port, err := net.SplitHostPort(t)
	if err != nil {
		return "", err
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

func (f *forwardServer) Open(st pb.Forward_OpenServer) error {
	first, err := st.Recv()
	if err != nil {
		return err
	}
	target, err := forwardTarget(first.Target)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "target %q: %v", first.Target, err)
	}
	if err := f.s.forwardAllowed(st.Context(), target); err != nil {
		f.s.Audit.Log(IdentityFrom(st.Context()), pb.Forward_Open_FullMethodName, map[string]any{"target": target, "denied": status.Convert(err).Message()})
		return err
	}
	f.s.Audit.Log(IdentityFrom(st.Context()), pb.Forward_Open_FullMethodName, map[string]any{"target": target})
	c, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		return status.Errorf(codes.Unavailable, "connect %s: %v", target, err)
	}
	defer c.Close()
	if len(first.Data) > 0 {
		if _, err := c.Write(first.Data); err != nil {
			return status.Errorf(codes.Unavailable, "write %s: %v", target, err)
		}
	}
	return pipeStream(st, c, first.Eof)
}

func (f *forwardServer) Ssh(st pb.Forward_SshServer) error {
	if f.s.SSH == nil {
		return status.Error(codes.Unimplemented, "the built-in sshd is disabled on this worker")
	}
	first, err := st.Recv()
	if err != nil {
		return err
	}
	id := IdentityFrom(st.Context())
	a, b := net.Pipe()
	defer a.Close()
	go f.s.SSH.HandleProxyConn(b, id)
	if len(first.Data) > 0 {
		if _, err := a.Write(first.Data); err != nil {
			return status.Error(codes.Unavailable, err.Error())
		}
	}
	return pipeStream(st, a, first.Eof)
}

type frameStream interface {
	Send(*pb.ForwardFrame) error
	Recv() (*pb.ForwardFrame, error)
}

// closeWrite half-closes c when it supports it, else closes it.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}

// pipeStream copies between a forward stream and a connection, keeping TCP
// half-close semantics in both directions. It returns once the connection's
// read side ended (eof sent) and the client finished sending.
func pipeStream(st frameStream, c net.Conn, clientDone bool) error {
	var wg sync.WaitGroup
	if clientDone {
		closeWrite(c)
	} else {
		wg.Add(1)
		go func() { // client -> conn
			defer wg.Done()
			for {
				m, err := st.Recv()
				if err != nil {
					if errors.Is(err, io.EOF) {
						closeWrite(c)
					} else {
						c.Close() // client gone: tear down
					}
					return
				}
				if len(m.Data) > 0 {
					if _, err := c.Write(m.Data); err != nil {
						c.Close()
						drain(st)
						return
					}
				}
				if m.Eof {
					closeWrite(c)
					drain(st)
					return
				}
			}
		}()
	}
	buf := make([]byte, 32<<10)
	var sendErr error
	for {
		n, err := c.Read(buf)
		if n > 0 {
			if sendErr = st.Send(&pb.ForwardFrame{Data: buf[:n]}); sendErr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	if sendErr == nil {
		sendErr = st.Send(&pb.ForwardFrame{Eof: true})
	}
	if sendErr != nil {
		c.Close()
	}
	wg.Wait()
	return nil
}

// drain discards client frames until the client closes its side.
func drain(st frameStream) {
	for {
		if _, err := st.Recv(); err != nil {
			return
		}
	}
}
