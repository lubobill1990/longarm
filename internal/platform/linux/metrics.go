//go:build linux

package linux

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lubobill1990/longarm/internal/platform/ptypes"
)

// clkTck is USER_HZ; 100 on every mainstream Linux (sysconf needs cgo).
const clkTck = 100

// SampleSystem reads /proc/stat, /proc/meminfo and /proc/uptime.
func SampleSystem() (ptypes.SysSample, error) {
	var s ptypes.SysSample
	s.NumCPU = runtime.NumCPU()
	f, err := os.Open("/proc/stat")
	if err != nil {
		return s, err
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		// user nice system idle iowait irq softirq steal guest guest_nice
		fs := strings.Fields(line)[1:]
		for i, v := range fs {
			if i >= 8 { // guest time is already included in user/nice
				break
			}
			n, _ := strconv.ParseUint(v, 10, 64)
			s.TotalTicks += n
			if i == 3 || i == 4 {
				s.IdleTicks += n
			}
		}
		break
	}
	f.Close()
	mi, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return s, err
	}
	for _, line := range strings.Split(string(mi), "\n") {
		fs := strings.Fields(line)
		if len(fs) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fs[1], 10, 64)
		switch fs[0] {
		case "MemTotal:":
			s.MemTotal = v * 1024
		case "MemAvailable:":
			s.MemAvail = v * 1024
		}
	}
	if up, err := os.ReadFile("/proc/uptime"); err == nil {
		if fs := strings.Fields(string(up)); len(fs) > 0 {
			sec, _ := strconv.ParseFloat(fs[0], 64)
			s.Uptime = time.Duration(sec * float64(time.Second))
		}
	}
	return s, nil
}

// ListProcesses walks /proc.
func ListProcesses() ([]ptypes.ProcSample, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	page := uint64(os.Getpagesize())
	out := make([]ptypes.ProcSample, 0, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, f, err := procStat(pid)
		if err != nil || len(f) < 22 {
			continue
		}
		ppid, _ := strconv.Atoi(string(f[1]))
		ut, _ := strconv.ParseUint(string(f[11]), 10, 64)
		st, _ := strconv.ParseUint(string(f[12]), 10, 64)
		start, _ := strconv.ParseUint(string(f[19]), 10, 64)
		rssPages, _ := strconv.ParseUint(string(f[21]), 10, 64)
		out = append(out, ptypes.ProcSample{
			PID: pid, PPID: ppid, Name: comm,
			CPUTime:   time.Duration(ut+st) * time.Second / clkTck,
			RSS:       rssPages * page,
			StartTime: start,
		})
	}
	return out, nil
}
