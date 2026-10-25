//go:build windows

package windows

import (
	"context"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// ListenLocal listens on the named pipe \\.\pipe\longarm-<name>, accessible
// only to the current user (and SYSTEM). Returns the pipe path as address.
func ListenLocal(name string) (net.Listener, string, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, "", err
	}
	addr := `\\.\pipe\longarm-` + name
	ln, err := winio.ListenPipe(addr, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)",
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		return nil, "", err
	}
	return ln, addr, nil
}

// DialLocal connects to an address returned by ListenLocal.
func DialLocal(ctx context.Context, addr string) (net.Conn, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	return winio.DialPipeContext(ctx, addr)
}

func currentUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}
