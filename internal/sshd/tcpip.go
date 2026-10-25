package sshd

import (
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// directTCPIPHandler serves direct-tcpip channels (ssh -L / -D, VS Code
// Remote-SSH) like gliderlabs' DirectTCPIPHandler, but keeps TCP half-close:
// the client's EOF becomes CloseWrite on the target and the target's EOF a
// channel CloseWrite, instead of tearing both directions down at once.
func directTCPIPHandler(srv *ssh.Server, conn *gossh.ServerConn, newChan gossh.NewChannel, ctx ssh.Context) {
	var d struct {
		DestAddr   string
		DestPort   uint32
		OriginAddr string
		OriginPort uint32
	}
	if err := gossh.Unmarshal(newChan.ExtraData(), &d); err != nil {
		newChan.Reject(gossh.ConnectionFailed, "error parsing forward data: "+err.Error())
		return
	}
	if srv.LocalPortForwardingCallback == nil || !srv.LocalPortForwardingCallback(ctx, d.DestAddr, d.DestPort) {
		newChan.Reject(gossh.Prohibited, "port forwarding is disabled")
		return
	}
	var dialer net.Dialer
	dconn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(d.DestAddr, strconv.FormatInt(int64(d.DestPort), 10)))
	if err != nil {
		newChan.Reject(gossh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newChan.Accept()
	if err != nil {
		dconn.Close()
		return
	}
	go gossh.DiscardRequests(reqs)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(ch, dconn)
		ch.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		io.Copy(dconn, ch)
		if tc, ok := dconn.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dconn.Close()
		}
	}()
	go func() {
		wg.Wait()
		ch.Close()
		dconn.Close()
	}()
}
