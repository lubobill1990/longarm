//go:build !windows

package updatesvc

import (
	"context"
	"net"
)

// The service is Windows-only (MSI installs); elsewhere there is nothing
// to talk to.

func dialService(context.Context) (net.Conn, error) { return nil, ErrNoService }

// VerifyPipeServer: no service here.
func VerifyPipeServer(net.Conn) error { return ErrNoService }

// Installed: never outside Windows.
func Installed() bool { return false }

// MayChangeAuto: no service here.
func MayChangeAuto() bool { return false }
