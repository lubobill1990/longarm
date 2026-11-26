//go:build !windows

package desktopsvc

import (
	"context"
	"log/slog"
	"os"

	pb "longarm/proto/longarm/v1"
)

func RunService(context.Context, string, *slog.Logger) error { return ErrUnsupported }
func RunAgent(context.Context, *slog.Logger) error           { return ErrUnsupported }
func Install(string, []string) error                         { return ErrUnsupported }
func Uninstall() error                                       { return ErrUnsupported }
func Installed() bool                                        { return false }
func Clients() []string                                      { return nil }
func RunningAsSystem() bool                                  { return false }
func LogFile(string) (*os.File, error)                       { return nil, ErrUnsupported }

// Agent: no agent outside Windows.
func Agent(context.Context) (pb.RemoteDesktopClient, bool) { return nil, false }

func AccountSID(string) (string, error) { return "", ErrUnsupported }
func CurrentUserSID() string            { return "" }
