package main

// The terminal of the Sessions window (docs/agent-sessions-ui.md section
// 3.4): one Sessions.Attach stream per WebSocket, replay included.
//
//	server → client: binary = terminal output; text = {"type":"started","session":{...}},
//	                 {"type":"exit","code":N} (then close 1000), {"type":"error","message":"..."} (then close 1011)
//	client → server: binary = keyboard input; text = {"type":"resize","cols":C,"rows":R}, {"type":"detach"}
//
// Closing the WebSocket detaches: the session keeps running.

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

	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

const (
	wsPingEvery = 20 * time.Second
	// wsWriteStall: a client that takes longer for a frame is dropped (1008);
	// it attaches again and the replay restores its screen.
	wsWriteStall = 10 * time.Second
	wsReadLimit  = 1 << 20 // a paste
)

// attachWS is GET /v1/workers/{worker}/sessions/{name}/attach?cols=&rows=
// and GET /v1/workers/{worker}/jobs/{id}/attach (a steerable run).
func (d *daemon) attachWS(w http.ResponseWriter, r *http.Request) {
	conn, err := d.conn(r.PathValue("worker"))
	if err != nil {
		agentError(w, err)
		return
	}
	open := &pb.AttachOpen{Name: r.PathValue("name"), NoCreate: true}
	if id := r.PathValue("id"); id != "" {
		open = &pb.AttachOpen{Name: id, Job: true, NoCreate: true}
	}
	cols, _ := strconv.ParseUint(r.URL.Query().Get("cols"), 10, 16)
	rows, _ := strconv.ParseUint(r.URL.Query().Get("rows"), 10, 16)
	if cols > 0 && rows > 0 {
		open.Size = &pb.TermSize{Cols: uint32(cols), Rows: uint32(rows)}
	}
	// any origin: the daemon's local API is the user's alone; the tray
	// checks the browser's origin
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return // Accept answered
	}
	ws.SetReadLimit(wsReadLimit)
	bridgeAttach(r.Context(), ws, conn.Sessions(), open)
}

// bridgeAttach attaches to a session and relays between ws and the worker
// until either side ends.
func bridgeAttach(ctx context.Context, ws *websocket.Conn, sc pb.SessionsClient, open *pb.AttachOpen) {
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
		wsFail(ctx, ws, err)
		return
	}
	ss, err := agentMO.Marshal(first.GetStarted().GetSession())
	if err != nil || !wsControl(ctx, ws, map[string]any{"type": "started", "session": json.RawMessage(ss)}) {
		return
	}
	go wsKeepalive(ctx, ws, cancel)
	go wsInput(ctx, ws, st, cancel)
	wsOutput(ctx, ws, st)
}

// wsOutput relays the worker's output to the client.
func wsOutput(ctx context.Context, ws *websocket.Conn, st pb.Sessions_AttachClient) {
	for {
		out, err := st.Recv()
		switch {
		case ctx.Err() != nil: // the client went
			return
		case errors.Is(err, io.EOF): // detached
			ws.Close(websocket.StatusNormalClosure, "detached")
			return
		case err != nil:
			wsFail(ctx, ws, err)
			return
		}
		switch m := out.Msg.(type) {
		case *pb.AttachOutput_Data:
			if !wsWrite(ctx, ws, websocket.MessageBinary, m.Data) {
				return
			}
		case *pb.AttachOutput_ExitCode:
			if wsControl(ctx, ws, map[string]any{"type": "exit", "code": m.ExitCode}) {
				ws.Close(websocket.StatusNormalClosure, "exited")
			}
			return
		}
	}
}

// wsInput relays the client's keyboard input and control messages to the
// worker; the client going (or failing) ends the attach.
func wsInput(ctx context.Context, ws *websocket.Conn, st pb.Sessions_AttachClient, cancel context.CancelFunc) {
	for {
		typ, b, err := ws.Read(ctx)
		if err != nil {
			cancel()
			return
		}
		if typ == websocket.MessageBinary {
			if st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Data{Data: b}}) != nil {
				return // wsOutput sees why
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
		case "detach": // the worker ends the stream; wsOutput closes
			st.Send(&pb.AttachInput{Msg: &pb.AttachInput_Detach{Detach: true}})
			st.CloseSend()
			return
		}
	}
}

// wsKeepalive pings the client; one that does not answer ends the attach.
func wsKeepalive(ctx context.Context, ws *websocket.Conn, cancel context.CancelFunc) {
	t := time.NewTicker(wsPingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c, done := context.WithTimeout(ctx, wsPingEvery)
		err := ws.Ping(c)
		done()
		if err != nil {
			cancel()
			return
		}
	}
}

// wsWrite writes one frame; a client that does not take it within
// wsWriteStall is closed with 1008.
func wsWrite(ctx context.Context, ws *websocket.Conn, typ websocket.MessageType, b []byte) bool {
	t := time.AfterFunc(wsWriteStall, func() { ws.Close(websocket.StatusPolicyViolation, "client too slow") })
	defer t.Stop()
	return ws.Write(ctx, typ, b) == nil
}

func wsControl(ctx context.Context, ws *websocket.Conn, v any) bool {
	b, _ := json.Marshal(v)
	return wsWrite(ctx, ws, websocket.MessageText, b)
}

// wsFail reports err to the client and closes with 1011.
func wsFail(ctx context.Context, ws *websocket.Conn, err error) {
	if wsControl(ctx, ws, map[string]any{"type": "error", "message": status.Convert(err).Message()}) {
		ws.Close(websocket.StatusInternalError, "")
	}
}
