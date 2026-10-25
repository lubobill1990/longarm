//go:build windows

package windows

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ProcessStartTime returns the process creation time as a FILETIME value
// (100 ns ticks since 1601). Together with the pid it identifies a process
// instance, guarding against pid reuse.
func ProcessStartTime(pid int) (uint64, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(h)
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return 0, err
	}
	return uint64(c.HighDateTime)<<32 | uint64(c.LowDateTime), nil
}

// ProcessAlive reports whether pid is running and (if start != 0) is the same
// process instance that had creation time start.
func ProcessAlive(pid int, start uint64) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Access denied means it exists but belongs to someone else; we only
		// track our own processes, so treat it as "not ours" unless no start
		// time is requested.
		return errors.Is(err, windows.ERROR_ACCESS_DENIED) && start == 0
	}
	defer windows.CloseHandle(h)
	if ev, _ := windows.WaitForSingleObject(h, 0); ev == uint32(windows.WAIT_OBJECT_0) {
		return false // exited (handle still referenced somewhere)
	}
	if start == 0 {
		return true
	}
	var c, e, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &e, &k, &u); err != nil {
		return false
	}
	return uint64(c.HighDateTime)<<32|uint64(c.LowDateTime) == start
}

// KillProcess terminates a single process. On Windows there is no process
// group to kill; trees are handled by Job Objects (Container).
func KillProcess(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, KilledExitCode)
}

const (
	detachedProcess        = 0x00000008
	createBreakawayFromJob = 0x01000000
)

// StartDetached starts cmd so that it outlives the caller: no console, its
// own process group, and - when the caller itself runs inside a Job Object
// that allows it - broken away from that job (so e.g. a test harness or a
// terminal closing does not take the shim down). Falls back to a plain
// detached start if breakaway is not permitted. newCmd is called once per
// attempt because an exec.Cmd cannot be started twice.
func StartDetached(newCmd func() *exec.Cmd) (*exec.Cmd, error) {
	base := uint32(detachedProcess | createNewProcessGroup)
	start := func(flags uint32) (*exec.Cmd, error) {
		cmd := newCmd()
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
		return cmd, cmd.Start()
	}
	cmd, err := start(base | createBreakawayFromJob)
	if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return cmd, err
	}
	return start(base)
}

// LockFile takes an exclusive, non-blocking lock on f.
func LockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}

// DiskUsage returns free (available to caller) and total bytes of the volume
// holding path.
func DiskUsage(path string) (free, total uint64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var all uint64
	err = windows.GetDiskFreeSpaceEx(p, &free, &total, &all)
	return free, total, err
}

// ReadFileShared reads a whole file opened with FILE_SHARE_DELETE as well as
// read/write sharing, so a concurrent atomic replace (rename over it) by the
// shim does not fail because longarmd happens to be reading it.
func ReadFileShared(path string) ([]byte, error) {
	f, err := OpenShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// OpenShared opens path read-only with full sharing (read, write, delete).
func OpenShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// fileRenameInfo is FILE_RENAME_INFO (the Ex variant uses the Flags member).
type fileRenameInfo struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

const (
	fileRenameInfoEx              = 22
	fileRenameFlagReplaceIfExists = 0x1
	fileRenameFlagPosixSemantics  = 0x2
	fileRenameFlagIgnoreReadOnly  = 0x40
)

// posixRename renames with POSIX semantics (Windows 10 1607+): the target is
// replaced even while readers that opened it with FILE_SHARE_DELETE (like
// OpenShared) still have it open - they keep reading the old content.
func posixRename(src, dst string) error {
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	name, err := windows.UTF16FromString(absDst)
	if err != nil {
		return err
	}
	name = name[:len(name)-1] // no terminator needed; length is explicit
	size := int(unsafe.Offsetof(fileRenameInfo{}.FileName)) + (len(name)+1)*2
	buf := make([]byte, size+8)
	info := (*fileRenameInfo)(unsafe.Pointer(&buf[0]))
	info.Flags = fileRenameFlagReplaceIfExists | fileRenameFlagPosixSemantics | fileRenameFlagIgnoreReadOnly
	info.FileNameLength = uint32(len(name) * 2)
	copy(unsafe.Slice(&info.FileName[0], len(name)), name)
	return windows.SetFileInformationByHandle(h, fileRenameInfoEx, &buf[0], uint32(size))
}

// ReplaceFile atomically renames src over dst. It first tries a POSIX
// rename; if the filesystem or OS does not support it, it falls back to
// MoveFileEx, retrying briefly on sharing violations caused by readers that
// did not open with FILE_SHARE_DELETE (e.g. other tools reading the compat
// files).
func ReplaceFile(src, dst string) error {
	if posixRename(src, dst) == nil {
		return nil
	}
	var err error
	for i := 0; i < 50; i++ {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}
