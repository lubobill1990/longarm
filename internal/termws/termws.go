// Package termws bridges a browser terminal's WebSocket to a worker's
// Sessions.Attach stream (docs/agent-sessions-ui.md section 3.4): the
// longarm daemon's Sessions window and the phone app (internal/mobile)
// attach with it.
//
//	server → client: binary = terminal output; text = {"type":"started","session":{...}},
//	                 {"type":"exit","code":N} (then close 1000), {"type":"error","message":"..."} (then close 1011)
//	client → server: binary = keyboard input; text = {"type":"resize","cols":C,"rows":R}, {"type":"detach"}
//
// Closing the WebSocket detaches: the session keeps running.
package termws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	pb "longarm/proto/longarm/v1"
)

const (
	pingEvery = 20 * time.Second
	// writeStall: a client that takes longer for a frame is dropped (1008);
	// it attaches again and the replay restores its screen.
	writeStall = 10 * time.Second
	readLimit  = 1 << 20 // a paste
)

var mo = protojson.MarshalOptions{UseProtoNames: true}

// Open is the attach the request asks for: session name (or a steerable
// job's id with job), the size from ?cols=&rows=.
func Open(r *http.Request, name string, job bool) *pb.AttachOpen {
	open := &pb.AttachOpen{Name: name, Job: job, NoCreate: true}
	cols, _ := strconv.ParseUint(r.URL.Query().Get("cols"), 10, 16)
	rows, _ := strconv.ParseUint(r.URL.Query().Get("rows"), 10, 16)
	if cols > 0 && rows > 0 {
		open.Size = &pb.TermSize{Cols: uint32(cols), Rows: uint32(rows)}
	}
	return open
}

// Serve accepts the WebSocket (any origin: the caller checked who may) and
// bridges it to the attach until either side ends.
func Serve(w http.ResponseWriter, r *http.Request, sc pb.SessionsClient, open *pb.AttachOpen) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return // Accept answered
	}
	ws.SetReadLimit(readLimit)
	Bridge(r.Context(), ws, sc, open)
}

// Bridge attaches to a session and relays between ws and the worker until
// either side ends.
func Bridge(ctx context.Context, ws *websocket.Conn, sc pb.SessionsClient, open *pb.AttachOpen) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer ws.CloseNow()
	st, err := sc.Attach(ctx)
	if err == nil {
		err = st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Open{Open: open}})
	}
	var first *pb.AttachOutput
	if err == nil {
		first, err = st.Recv()
	}
	if err == nil && first.GetStarted() == nil {
		err = errors.New("the worker did not start the attach")
	}
	if err != nil {
		Fail(ctx, ws, err)
		return
	}
	ss, err := mo.Marshal(first.GetStarted().GetSession())
	if err != nil || !control(ctx, ws, map[string]any{"type": "started", "session": json.RawMessage(ss)}) {
		return
	}
	go keepalive(ctx, ws, cancel)
	go input(ctx, ws, st, cancel)
	output(ctx, ws, st)
}

// output relays the worker's output to the client.
func output(ctx context.Context, ws *websocket.Conn, st pb.Sessions_AttachClient) {
	for {
		out, err := st.Recv()
		switch {
		case ctx.Err() != nil: // the client went
			return
		case errors.Is(err, io.EOF): // detached
			ws.Close(websocket.StatusNormalClosure, "detached")
			return
		case err != nil:
			Fail(ctx, ws, err)
			return
		}
		switch m := out.Msg.(type) {
		case *pb.AttachOutput_Data:
			if !write(ctx, ws, websocket.MessageBinary, m.Data) {
				return
			}
		case *pb.AttachOutput_ExitCode:
			if control(ctx, ws, map[string]any{"type": "exit", "code": m.ExitCode}) {
				ws.Close(websocket.StatusNormalClosure, "exited")
			}
			return
		}
	}
}

// input relays the client's keyboard input and control messages to the
// worker; the client going (or failing) ends the attach.
func input(ctx context.Context, ws *websocket.Conn, st pb.Sessions_AttachClient, cancel context.CancelFunc) {
	for {
		typ, b, err := ws.Read(ctx)
		if err != nil {
			cancel()
			return
		}
		if typ == websocket.MessageBinary {
			if st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Data{Data: b}}) != nil {
				return // output sees why
			}
			continue
		}
		var c struct {
			Type       string `json:"type"`
			Cols, Rows uint32
		}
		if json.Unmarshal(b, &c) != nil {
			continue
		}
		switch c.Type {
		case "resize":
			if c.Cols > 0 && c.Rows > 0 {
				st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Resize{Resize: &pb.TermSize{Cols: c.Cols, Rows: c.Rows}}})
			}
		case "detach": // the worker ends the stream; output closes
			st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Detach{Detach: true}})
			st.CloseSend()
			return
		}
	}
}

// keepalive pings the client; one that does not answer ends the attach.
func keepalive(ctx context.Context, ws *websocket.Conn, cancel context.CancelFunc) {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c, done := context.WithTimeout(ctx, pingEvery)
		err := ws.Ping(c)
		done()
		if err != nil {
			cancel()
			return
		}
	}
}

// write writes one frame; a client that does not take it within writeStall
// is closed with 1008.
func write(ctx context.Context, ws *websocket.Conn, typ websocket.MessageType, b []byte) bool {
	t := time.AfterFunc(writeStall, func() { ws.Close(websocket.StatusPolicyViolation, "client too slow") })
	defer t.Stop()
	return ws.Write(ctx, typ, b) == nil
}

func control(ctx context.Context, ws *websocket.Conn, v any) bool {
	b, _ := json.Marshal(v)
	return write(ctx, ws, websocket.MessageText, b)
}

// Fail reports err to the client and closes with 1011.
func Fail(ctx context.Context, ws *websocket.Conn, err error) {
	if control(ctx, ws, map[string]any{"type": "error", "message": status.Convert(err).Message()}) {
		ws.Close(websocket.StatusInternalError, "")
	}
}
