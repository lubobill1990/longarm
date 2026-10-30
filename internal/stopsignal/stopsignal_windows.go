//go:build windows

package stopsignal

import (
	"context"
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NotifyContext returns ctx, canceled as well when the installer sets this
// process's stop event (Name(os.Getpid())). Without the event (name taken,
// no rights) it is ctx with a cancel func: the process is then terminated
// instead of asked.
func NotifyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	ev, err := create(os.Getpid())
	if err != nil {
		return ctx, cancel
	}
	go func() {
		defer windows.CloseHandle(ev)
		for ctx.Err() == nil {
			r, err := windows.WaitForSingleObject(ev, 500)
			if err != nil {
				return
			}
			if r == windows.WAIT_OBJECT_0 {
				cancel()
				return
			}
		}
	}()
	return ctx, cancel
}

// create makes the event: this user, SYSTEM and Administrators only. An
// existing object of that name (made by someone else for a pid to come) is
// refused.
func create(pid int) (windows.Handle, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return 0, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + u.User.Sid.String() + ")")
	if err != nil {
		return 0, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	name, err := windows.UTF16PtrFromString(Name(pid))
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateEvent(sa, 1, 0, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return 0, err
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return 0, errors.New("stop event name taken")
	}
	return h, nil
}

// Signal sets process pid's stop event; false when there is none (a
// process without graceful stop: an older version).
func Signal(pid int) (bool, error) {
	name, err := windows.UTF16PtrFromString(Name(pid))
	if err != nil {
		return false, err
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return false, nil
		}
		return false, err
	}
	defer windows.CloseHandle(h)
	if err := windows.SetEvent(h); err != nil {
		return false, err
	}
	return true, nil
}

func closeHandle(h windows.Handle) { windows.CloseHandle(h) }
