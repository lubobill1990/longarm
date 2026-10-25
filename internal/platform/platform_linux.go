//go:build linux

package platform

import (
	"context"

	impl "github.com/lubobill1990/longarm/internal/platform/linux"
)

func NewContainer(opts ContainerOptions) (Container, error) { return impl.NewContainer(opts) }

func ProcessStartTime(pid int) (uint64, error) { return impl.ProcessStartTime(pid) }
func ProcessAlive(pid int, start uint64) bool  { return impl.ProcessAlive(pid, start) }

func SampleSystem() (SysSample, error)     { return impl.SampleSystem() }
func ListProcesses() ([]ProcSample, error) { return impl.ListProcesses() }

func InstallService(cfg ServiceConfig) error          { return impl.InstallService(cfg) }
func UninstallService(name string) error              { return impl.UninstallService(name) }
func StartService(name string) error                  { return impl.StartService(name) }
func StopService(name string) error                   { return impl.StopService(name) }
func QueryService(name string) (ServiceStatus, error) { return impl.QueryService(name) }
func RunAsService(name string, fn func(context.Context) error) (bool, error) {
	return impl.RunAsService(name, fn)
}
