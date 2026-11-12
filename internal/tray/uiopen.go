package tray

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"longarm/internal/localapi"
	"longarm/internal/uipage"
)

// `longarm ui open` (internal/uipage): the tray serves a control channel
// only this user can reach, and opens the page it is asked for - the
// popup at a tab (ShowPopup) or the Sessions window (OpenSessions) - or
// answers a URL with a one-time code for a program's own webview.

// ServeControl serves the control channel (its address in
// <config dir>/tray.addr) until ctx is done. Another tray already serving
// it keeps it: ServeControl returns at once.
func (s *Server) ServeControl(ctx context.Context) error {
	addrFile := uipage.AddrFile(s.opts.LongarmConfigDir)
	if _, err := uipage.Open(ctx, addrFile, uipage.Request{Page: "-"}); !errors.Is(err, uipage.ErrNotRunning) {
		return errors.New("another longarm-tray serves " + addrFile)
	}
	ch, err := localapi.ListenChannel("tray", s.opts.LongarmConfigDir, addrFile)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.ControlHandler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		ch.Release()
		srv.Close()
	}()
	if err := srv.Serve(ch.Listener); !errors.Is(err, http.ErrServerClosed) {
		ch.Release()
		return err
	}
	return nil
}

// ControlHandler is the control channel's API: POST /ui/open.
func (s *Server) ControlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ui/open", func(w http.ResponseWriter, r *http.Request) {
		var req uipage.Request
		if !readBody(w, r, &req) {
			return
		}
		res, err := s.OpenPage(req)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, res)
	})
	return mux
}

// OpenPage opens req's page (or, PrintURL, mints a URL for it).
func (s *Server) OpenPage(req uipage.Request) (uipage.Result, error) {
	pg, err := uipage.Resolve(req.Page, req.Target)
	if err != nil {
		return uipage.Result{}, err
	}
	if req.PrintURL {
		return uipage.Result{URL: s.pageURL(pg, true)}, nil
	}
	if err := s.showPage(pg); err != nil {
		return uipage.Result{}, err
	}
	return uipage.Result{URL: s.pageURL(pg, false), Opened: true}, nil
}

// pageURL is pg on this server; fresh: with a one-time bootstrap code.
func (s *Server) pageURL(pg uipage.Page, fresh bool) string {
	if pg.Path == uipage.SessionsPath {
		return s.SessionsURL(pg.Hash, fresh)
	}
	if fresh {
		return s.URL(pg.Path)
	}
	return "http://" + s.Addr() + pg.Path
}

// showPage shows pg in the shell's windows, else in the default browser.
func (s *Server) showPage(pg uipage.Page) error {
	if pg.Path == uipage.SessionsPath {
		if s.OpenSessions != nil {
			s.OpenSessions(pg.Hash)
			return nil
		}
		return s.openURL(s.SessionsURL(pg.Hash, true))
	}
	if s.ShowPopup != nil && strings.HasPrefix(pg.Path, "/tray") {
		s.ShowPopup(pg.Path)
		return nil
	}
	return s.openURL(s.URL(pg.Path))
}
