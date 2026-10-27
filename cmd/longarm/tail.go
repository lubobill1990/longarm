package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/lubobill1990/longarm/internal/client"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// tailLine is one record of tail --json: a line of the file (without its
// newline) and the offsets around it; --from-offset next resumes after it.
type tailLine struct {
	Offset    int64  `json:"offset"`
	Next      int64  `json:"next"`
	Line      string `json:"line"`
	Partial   bool   `json:"partial,omitempty"`   // the file's last line had no newline yet (without -f)
	Truncated bool   `json:"truncated,omitempty"` // the file shrank or was replaced: reading again from offset
}

// tailWriter turns chunks into output (raw bytes, or JSON lines).
type tailWriter struct {
	w       io.Writer
	asJSON  bool
	next    int64 // offset after the last byte received
	pending []byte
	pendAt  int64
}

func (t *tailWriter) emit(l tailLine) error {
	b, _ := json.Marshal(l)
	_, err := t.w.Write(append(b, '\n'))
	return err
}

func (t *tailWriter) chunk(c *pb.FileChunk) error {
	if c.Offset < t.next { // truncated or replaced
		t.pending = nil
		if t.asJSON {
			if err := t.emit(tailLine{Offset: c.Offset, Next: c.Offset, Truncated: true}); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(os.Stderr, "longarm: the file was truncated or replaced; reading again from offset %d\n", c.Offset)
		}
	}
	t.next = c.Offset + int64(len(c.Data))
	if !t.asJSON {
		_, err := t.w.Write(c.Data)
		return err
	}
	if len(t.pending) == 0 {
		t.pendAt = c.Offset
	}
	t.pending = append(t.pending, c.Data...)
	for {
		i := bytes.IndexByte(t.pending, '\n')
		if i < 0 {
			return nil
		}
		line := bytes.TrimSuffix(t.pending[:i], []byte{'\r'})
		if err := t.emit(tailLine{Offset: t.pendAt, Next: t.pendAt + int64(i) + 1, Line: string(line)}); err != nil {
			return err
		}
		t.pendAt += int64(i) + 1
		t.pending = t.pending[i+1:]
	}
}

// flush prints an unterminated last line (without -f).
func (t *tailWriter) flush() error {
	if !t.asJSON || len(t.pending) == 0 {
		return nil
	}
	return t.emit(tailLine{Offset: t.pendAt, Next: t.pendAt + int64(len(t.pending)), Line: string(t.pending), Partial: true})
}

func tailCmd() *cobra.Command {
	var follow, asJSON bool
	var from int64
	c := &cobra.Command{
		Use:   "tail <worker> <path> [-f] [--from-offset N] [--json]",
		Short: "Print a file on a worker from an offset; -f streams what is appended (same access as get)",
		Long: `Print a worker file from a byte offset (negative: that many bytes before the
end); with -f keep streaming what is appended until interrupted, waiting for
a file that does not exist yet and reading a truncated or replaced file
again from its start. Paths and access are those of longarm get. With -f
longarm reconnects after network errors, resuming at the last byte.

--json prints one object per line of the file:
{"offset": N, "next": M, "line": "..."}; "next" is where --from-offset
resumes after that line. A file that shrank or was replaced gives
{"truncated": true, "offset": 0, ...}; without -f an unterminated last line
comes with "partial": true. Exit 3: no such file (without -f).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			conn, err := s.conn(args[0])
			if err != nil {
				return err
			}
			defer conn.Close()
			tw := &tailWriter{w: os.Stdout, asJSON: asJSON, next: max(from, 0)}
			off := from
			once := func(ctx context.Context, progress func()) error {
				st, err := conn.Files().Tail(ctx, &pb.FileTailRequest{Path: args[1], Offset: off, Follow: follow})
				if err != nil {
					return err
				}
				for {
					c, err := st.Recv()
					if errors.Is(err, io.EOF) {
						return nil
					}
					if err != nil {
						return err
					}
					progress()
					if off < 0 { // the first chunk tells where "before the end" is
						tw.next = c.Offset
					}
					if err := tw.chunk(c); err != nil {
						return err
					}
					off = tw.next
				}
			}
			if follow {
				err = client.Retry(cmd.Context(), 10*time.Minute, notifier(args[0]), once)
			} else {
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
				err = once(ctx, func() {})
				cancel()
			}
			if err != nil {
				return tooOld(err, conn, "tail")
			}
			return tw.flush()
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming appended bytes (reconnects automatically)")
	c.Flags().Int64Var(&from, "from-offset", 0, "start at this byte offset (negative: that many bytes before the end)")
	c.Flags().BoolVar(&asJSON, "json", false, `one {"offset","next","line"} object per line`)
	return c
}
