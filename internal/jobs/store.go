package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/lubobill1990/longarm/internal/logfile"
	"github.com/lubobill1990/longarm/internal/platform"
)

// File names inside a job directory.
const (
	SpecFile   = "job.json"
	StatusFile = "status.json"
	StateFile  = "state"
	PIDFile    = "pid"
	ExitFile   = "exit"
	LogFile    = "out.log"
	ResultFile = "result.md"
	KillFile   = "kill"
	ShimLog    = "shim.log"
)

// Dir is a job directory.
type Dir string

func (d Dir) Path(name string) string { return filepath.Join(string(d), name) }

// ID returns the job id (the directory's base name).
func (d Dir) ID() string { return filepath.Base(string(d)) }

// WriteAtomic writes data to name via a temp file + rename, so readers never
// see a torn file.
func (d Dir) WriteAtomic(name string, data []byte) error {
	tmp, err := os.CreateTemp(string(d), "."+name+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := platform.ReplaceFile(tmp.Name(), d.Path(name)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// WriteSpec writes job.json.
func (d Dir) WriteSpec(s *Spec) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return d.WriteAtomic(SpecFile, b)
}

// ReadSpec reads job.json.
func (d Dir) ReadSpec() (*Spec, error) {
	b, err := platform.ReadFileShared(d.Path(SpecFile))
	if err != nil {
		return nil, err
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", d.Path(SpecFile), err)
	}
	return &s, nil
}

// ReadStatus reads status.json.
func (d Dir) ReadStatus() (*Status, error) {
	b, err := platform.ReadFileShared(d.Path(StatusFile))
	if err != nil {
		return nil, err
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", d.Path(StatusFile), err)
	}
	return &s, nil
}

// WriteStatus bumps st.Seq, writes status.json and then the compat files
// (state, pid, exit) that existing tooling reads.
func (d Dir) WriteStatus(st *Status) error {
	st.Seq++
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := d.WriteAtomic(StatusFile, b); err != nil {
		return err
	}
	var errs []error
	if st.PID > 0 {
		errs = append(errs, d.WriteAtomic(PIDFile, []byte(strconv.Itoa(st.PID))))
	}
	if st.ExitCode != nil {
		errs = append(errs, d.WriteAtomic(ExitFile, []byte(strconv.Itoa(*st.ExitCode))))
	}
	errs = append(errs, d.WriteAtomic(StateFile, []byte(st.State)))
	return errors.Join(errs...)
}

// KillRequested reports whether the kill marker exists.
func (d Dir) KillRequested() bool {
	_, err := os.Stat(d.Path(KillFile))
	return err == nil
}

// RequestKill creates the kill marker.
func (d Dir) RequestKill(reason string) error {
	return os.WriteFile(d.Path(KillFile), []byte(reason), 0o600)
}

// LogSize returns the absolute end offset of out.log (0 if missing); see
// package logfile for rotation.
func (d Dir) LogSize() int64 { return logfile.Size(d.Path(LogFile)) }

// ReadLog reads up to max bytes of the log starting at absolute offset. A
// negative offset counts back from the current end. It returns the data, the
// offset the data starts at (after resolving/clamping; later than requested
// when those bytes were rotated away), and the log end observed. Offsets past
// the end yield no data and start == offset (the caller may wait for the log
// to grow).
func (d Dir) ReadLog(offset int64, max int) (data []byte, start, size int64, err error) {
	return logfile.Read(d.Path(LogFile), offset, max)
}
