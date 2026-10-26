package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// QueueFile is the queue in a telemetry dir: one JSON item per line,
// oldest first.
const QueueFile = "queue.jsonl"

const lockName = "queue.lock"

// Queue is the bounded disk queue of a telemetry dir. Several processes
// (CLI runs, longarm daemon, longarm mcp) share one: every operation holds
// a lock file. When full, the oldest items go and a telemetry.dropped
// metric item says how many.
type Queue struct {
	Dir      string
	MaxBytes int64
	MaxItems int
}

func (q *Queue) path() string { return filepath.Join(q.Dir, QueueFile) }

// errLocked: another process held the lock for too long.
var errLocked = errors.New("telemetry queue is locked")

// lock takes the queue's lock file (exclusive create). A lock older than
// 30 s belongs to a process that died holding it.
func (q *Queue) lock() (func(), error) {
	if err := os.MkdirAll(q.Dir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(q.Dir, lockName)
	deadline := time.Now().Add(3 * time.Second)
	for wait := time.Millisecond; ; {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.WriteString(strconv.Itoa(os.Getpid()))
			f.Close()
			return func() { os.Remove(p) }, nil
		}
		if !errors.Is(err, os.ErrExist) && !errors.Is(err, os.ErrPermission) {
			return nil, err // Windows reports a lock file being deleted as access denied
		}
		if fi, serr := os.Stat(p); serr == nil && time.Since(fi.ModTime()) > 30*time.Second {
			os.Remove(p)
			continue
		}
		if time.Now().After(deadline) {
			return nil, errLocked
		}
		time.Sleep(wait)
		if wait *= 2; wait > 50*time.Millisecond {
			wait = 50 * time.Millisecond
		}
	}
}

// read returns the queue's lines (unlocked).
func (q *Queue) read() ([][]byte, error) {
	b, err := os.ReadFile(q.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if l := bytes.TrimSpace(sc.Bytes()); len(l) > 0 {
			lines = append(lines, append([]byte(nil), l...))
		}
	}
	return lines, nil
}

// write replaces the queue with lines (unlocked).
func (q *Queue) write(lines [][]byte) error {
	if len(lines) == 0 {
		err := os.Remove(q.path())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var b bytes.Buffer
	for _, l := range lines {
		b.Write(l)
		b.WriteByte('\n')
	}
	return writeAtomic(q.path(), b.Bytes())
}

// Append adds items; when the queue would exceed its bounds the oldest
// items are dropped (returned: how many) and a telemetry.dropped metric
// item is added.
func (q *Queue) Append(items []Item) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	var add [][]byte
	var addBytes int64
	for _, it := range items {
		b, err := json.Marshal(it)
		if err != nil {
			continue
		}
		add = append(add, b)
		addBytes += int64(len(b)) + 1
	}
	unlock, err := q.lock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	var size int64
	if fi, err := os.Stat(q.path()); err == nil {
		size = fi.Size()
	}
	lines, err := q.read()
	if err != nil {
		return 0, err
	}
	if size+addBytes <= q.MaxBytes && len(lines)+len(add) <= q.MaxItems {
		f, err := os.OpenFile(q.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		var b bytes.Buffer
		for _, l := range add {
			b.Write(l)
			b.WriteByte('\n')
		}
		_, err = f.Write(b.Bytes())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return 0, err
	}
	// earlier telemetry.dropped items merge into the new one
	var all [][]byte
	prior := 0.0
	for _, l := range append(lines, add...) {
		if bytes.Contains(l, []byte(`"name":"`+MetricDropped+`"`)) {
			var it Item
			if json.Unmarshal(l, &it) == nil && it.Name == MetricDropped && it.Props["where"] == "queue" {
				prior += it.Value
				continue
			}
		}
		all = append(all, l)
	}
	// drop the oldest until it fits, keeping room for the dropped item
	dropped := 0
	total := int64(0)
	for _, l := range all {
		total += int64(len(l)) + 1
	}
	room := int64(512)
	for len(all) > 0 && (total+room > q.MaxBytes || len(all)+1 > q.MaxItems) {
		total -= int64(len(all[0])) + 1
		all = all[1:]
		dropped++
	}
	if n := float64(dropped) + prior; n > 0 {
		d := Item{Kind: KindMetric, Name: MetricDropped, Time: time.Now().UTC(), Value: n, Count: 1,
			Min: n, Max: n, Props: map[string]string{"where": "queue"}}
		if len(items) > 0 {
			d.Context = items[len(items)-1].Context
		}
		if b, err := json.Marshal(d); err == nil {
			all = append(all, b)
		}
	}
	return dropped, q.write(all)
}

// Take removes and returns up to max items (oldest first).
func (q *Queue) Take(max int) ([]Item, error) {
	unlock, err := q.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	lines, err := q.read()
	if err != nil || len(lines) == 0 {
		return nil, err
	}
	var out []Item
	n := 0
	for ; n < len(lines) && len(out) < max; n++ {
		var it Item
		if json.Unmarshal(lines[n], &it) == nil && it.Kind != "" {
			out = append(out, it)
		}
	}
	return out, q.write(lines[n:])
}

// Items returns the queued items without removing them.
func (q *Queue) Items() ([]Item, error) {
	lines, err := q.read()
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(lines))
	for _, l := range lines {
		var it Item
		if json.Unmarshal(l, &it) == nil && it.Kind != "" {
			out = append(out, it)
		}
	}
	return out, nil
}

// Stat returns the number of items and the size of the queue.
func (q *Queue) Stat() (int, int64) {
	fi, err := os.Stat(q.path())
	if err != nil {
		return 0, 0
	}
	lines, _ := q.read()
	return len(lines), fi.Size()
}

// Clear drops every queued item.
func (q *Queue) Clear() error {
	unlock, err := q.lock()
	if err != nil {
		return err
	}
	defer unlock()
	return q.write(nil)
}

func (q *Queue) String() string { return fmt.Sprintf("queue %s", q.path()) }
