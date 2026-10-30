package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lubobill1990/longarm/internal/client"
	"github.com/lubobill1990/longarm/internal/desktop"
	"github.com/lubobill1990/longarm/internal/devbox"
	"github.com/lubobill1990/longarm/internal/platform"
	pb "github.com/lubobill1990/longarm/proto/longarm/v1"
)

// desktopFile remembers which Dev Box each worker is and the Dev Center
// endpoints found (<config>/desktop.json).
const desktopFile = "desktop.json"

type desktopStore struct {
	// Endpoints: "<tenant>/<devcenter>" -> https://...devcenter.azure.com
	Endpoints map[string]string `json:"endpoints,omitempty"`
	// Workers: worker name -> its Dev Box (or a link set with --url).
	Workers map[string]desktopLink `json:"workers,omitempty"`
}

type desktopLink struct {
	DevCenter string    `json:"dev_center,omitempty"`
	Tenant    string    `json:"tenant,omitempty"`
	Project   string    `json:"project,omitempty"`
	DevBox    string    `json:"dev_box,omitempty"`
	URL       string    `json:"url"`               // ms-avd:connect?... (Windows App) or https
	WebURL    string    `json:"web_url,omitempty"` // the browser client
	SavedAt   time.Time `json:"saved_at"`
}

func loadDesktopStore() *desktopStore {
	st := &desktopStore{}
	if b, err := os.ReadFile(filepath.Join(configDir, desktopFile)); err == nil {
		json.Unmarshal(b, st)
	}
	if st.Endpoints == nil {
		st.Endpoints = map[string]string{}
	}
	if st.Workers == nil {
		st.Workers = map[string]desktopLink{}
	}
	return st
}

func (st *desktopStore) save() error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(configDir, desktopFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return platform.ReplaceFile(tmp, filepath.Join(configDir, desktopFile))
}

func desktopCmd() *cobra.Command {
	var printOnly, web, pick, forget bool
	var devBoxName, setURL string
	c := &cobra.Command{
		Use:   "desktop <worker>",
		Short: "Open the worker's desktop: Windows App for a Dev Box, Remote Desktop through a forward otherwise",
		Long: `Open the worker's desktop.

A Microsoft Dev Box opens in Windows App, which connects through Azure
Virtual Desktop itself (RDP Shortpath, Entra sign-in) - Longarm only finds
the link. It asks the Dev Center API with your Azure CLI sign-in
(az login --tenant <tenant>) which Dev Box the worker is; when you have
several, it asks once and remembers (<config>/desktop.json).

Any other Windows worker with Remote Desktop turned on is reached through a
forward to its Remote Desktop port and opened in mstsc (Windows App on
macOS); the forward ends when the window is closed. With per-user isolation
that needs an admin certificate (the port is the system's, not yours).

  longarm desktop w1                  # open it
  longarm desktop w1 --pick           # choose its Dev Box again
  longarm desktop w1 --devbox arm1    # say which Dev Box it is
  longarm desktop w1 --url 'ms-avd:connect?...'   # use this link
  longarm desktop w1 --web            # the browser client instead of Windows App
  longarm desktop w1 --print          # print the link (or the forward's address)
  longarm desktop w1 --forget         # forget its Dev Box`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			worker := args[0]
			st := loadDesktopStore()
			if forget {
				delete(st.Workers, worker)
				return st.save()
			}
			if setURL != "" {
				if !devbox.ValidLaunchURL(setURL) {
					return usagef("--url: want an ms-avd:connect?... or https:// link")
				}
				l := st.Workers[worker]
				l.URL, l.SavedAt = setURL, time.Now()
				st.Workers[worker] = l
				if err := st.save(); err != nil {
					return err
				}
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if l, ok := st.Workers[worker]; ok && l.URL != "" && !pick && devBoxName == "" {
				// Known: no need to reach the worker (a sleeping Dev Box
				// wakes up when Windows App connects).
				return launchDesktopLink(worker, l, web, printOnly)
			}
			s, err := openSession()
			if err != nil {
				return err
			}
			defer s.close()
			conn, err := s.conn(worker)
			if err != nil {
				return err
			}
			defer conn.Close()
			ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
			info, err := conn.Node().Info(ictx, &pb.InfoRequest{})
			cancel()
			if err != nil {
				return err
			}
			d := info.GetDesktop()
			if d == nil {
				return fmt.Errorf("%s runs longarmd %s, which does not report its desktop yet: update it (longarm worker update %s)", worker, info.Version, worker)
			}
			switch d.Kind {
			case desktop.KindDevBox:
				l, err := resolveDevBox(ctx, st, worker, d, devBoxName, pick)
				if err != nil {
					fmt.Fprintf(os.Stderr, "longarm: %s is a Dev Box (Dev Center %s), but its link could not be found: %s\n", worker, d.DevCenter, oneLine(err))
					fmt.Fprintf(os.Stderr, "longarm: open it from the Dev Box portal instead: %s\n", devbox.PortalURL)
					if printOnly {
						fmt.Println(devbox.PortalURL)
						return nil
					}
					return openDesktopURL(devbox.PortalURL)
				}
				return launchDesktopLink(worker, l, web, printOnly)
			case desktop.KindCloudPC:
				fmt.Fprintf(os.Stderr, "longarm: %s is a Windows 365 Cloud PC: open it from %s or Windows App\n", worker, devbox.CloudPCPortalURL)
				if printOnly {
					fmt.Println(devbox.CloudPCPortalURL)
					return nil
				}
				return openDesktopURL(devbox.CloudPCPortalURL)
			case desktop.KindRDP:
				return rdpDesktop(ctx, conn, worker, int(d.RdpPort), printOnly)
			}
			return fmt.Errorf("%s: no remote desktop (%s)", worker, nonEmptyStr(d.Reason, "unknown reason"))
		},
	}
	c.Flags().BoolVar(&printOnly, "print", false, "print the link (or the forward's address) instead of opening it")
	c.Flags().BoolVar(&web, "web", false, "Dev Box: open the browser client instead of Windows App")
	c.Flags().BoolVar(&pick, "pick", false, "Dev Box: choose which Dev Box the worker is again")
	c.Flags().StringVar(&devBoxName, "devbox", "", "Dev Box: the worker's Dev Box ([project/]name)")
	c.Flags().StringVar(&setURL, "url", "", "remember this link (ms-avd:connect?... or https://) for the worker and open it")
	c.Flags().BoolVar(&forget, "forget", false, "forget the worker's Dev Box and link")
	return c
}

func nonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func launchDesktopLink(worker string, l desktopLink, web, printOnly bool) error {
	u := l.URL
	if web {
		if l.WebURL == "" {
			return fmt.Errorf("no browser link saved for %s (longarm desktop %s --pick fetches it)", worker, worker)
		}
		u = l.WebURL
	}
	if !devbox.ValidLaunchURL(u) {
		return fmt.Errorf("saved link of %s is not usable: longarm desktop %s --forget", worker, worker)
	}
	if printOnly {
		fmt.Println(u)
		return nil
	}
	what := "Windows App"
	if strings.HasPrefix(u, "https:") {
		what = "the browser"
	}
	name := worker
	if l.DevBox != "" {
		name = fmt.Sprintf("%s (Dev Box %s/%s)", worker, l.Project, l.DevBox)
	}
	fmt.Fprintf(os.Stderr, "longarm: opening %s in %s\n", name, what)
	return openDesktopURL(u)
}

// resolveDevBox finds which of the user's Dev Boxes worker is and its link,
// and remembers both.
func resolveDevBox(ctx context.Context, st *desktopStore, worker string, d *pb.DesktopInfo, want string, pick bool) (desktopLink, error) {
	key := d.DevCenterTenant + "/" + d.DevCenter
	endpoint := st.Endpoints[key]
	if !devbox.ValidEndpoint(endpoint) {
		fmt.Fprintf(os.Stderr, "longarm: finding Dev Center %s...\n", d.DevCenter)
		e, err := devbox.FindEndpoint(ctx, devbox.DefaultResolver, d.DevCenterTenant, d.DevCenter)
		if err != nil {
			return desktopLink{}, err
		}
		endpoint = e
		st.Endpoints[key] = e
	}
	tok, err := devbox.AzToken(ctx, d.DevCenterTenant)
	if err != nil {
		return desktopLink{}, err
	}
	dc := &devbox.Client{Endpoint: endpoint, Token: tok}
	boxes, err := dc.DevBoxes(ctx)
	if err != nil {
		return desktopLink{}, err
	}
	if len(boxes) == 0 {
		return desktopLink{}, errors.New("you have no Dev Boxes in this Dev Center")
	}
	box, err := chooseDevBox(boxes, st, worker, d.DevCenter, want, pick, os.Stdin, os.Stderr, stdinIsTerminal())
	if err != nil {
		return desktopLink{}, err
	}
	rc, err := dc.RemoteConnection(ctx, box.ProjectName, box.Name)
	if err != nil {
		return desktopLink{}, err
	}
	l := desktopLink{DevCenter: d.DevCenter, Tenant: d.DevCenterTenant, Project: box.ProjectName, DevBox: box.Name,
		URL: rc.RDPConnectionURL, WebURL: rc.WebURL, SavedAt: time.Now()}
	if !devbox.ValidLaunchURL(l.URL) {
		l.URL = rc.WebURL
	}
	if !devbox.ValidLaunchURL(l.WebURL) {
		l.WebURL = ""
	}
	st.Workers[worker] = l
	if err := st.save(); err != nil {
		fmt.Fprintf(os.Stderr, "longarm: could not remember the Dev Box: %v\n", err)
	}
	return l, nil
}

var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// chooseDevBox picks worker's Dev Box: the one named by want; else the only
// one not already taken by another worker of the same Dev Center; else the
// user's choice (on a terminal).
func chooseDevBox(boxes []devbox.DevBox, st *desktopStore, worker, devCenter, want string, pick bool, in io.Reader, out io.Writer, tty bool) (devbox.DevBox, error) {
	if want != "" {
		project, name, ok := strings.Cut(want, "/")
		if !ok {
			project, name = "", want
		}
		var hits []devbox.DevBox
		for _, b := range boxes {
			if strings.EqualFold(b.Name, name) && (project == "" || strings.EqualFold(b.ProjectName, project)) {
				hits = append(hits, b)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0], nil
		case 0:
			return devbox.DevBox{}, fmt.Errorf("no Dev Box %q (yours: %s)", want, devBoxNames(boxes))
		}
		return devbox.DevBox{}, fmt.Errorf("Dev Box %q is in several projects: say which (--devbox <project>/%s)", want, name)
	}
	takenBy := map[string]string{}
	for w, l := range st.Workers {
		if w != worker && l.DevBox != "" && strings.EqualFold(l.DevCenter, devCenter) {
			takenBy[strings.ToLower(l.Project+"/"+l.DevBox)] = w
		}
	}
	var free []devbox.DevBox
	for _, b := range boxes {
		if takenBy[strings.ToLower(b.ProjectName+"/"+b.Name)] == "" {
			free = append(free, b)
		}
	}
	if len(free) == 1 && !pick {
		fmt.Fprintf(out, "longarm: %s is Dev Box %s/%s (your only one not linked to another worker)\n", worker, free[0].ProjectName, free[0].Name)
		return free[0], nil
	}
	if !tty {
		return devbox.DevBox{}, fmt.Errorf("which Dev Box is %s? say it with --devbox (yours: %s)", worker, devBoxNames(boxes))
	}
	fmt.Fprintf(out, "Which Dev Box is %s?\n", worker)
	for i, b := range boxes {
		note := ""
		if w := takenBy[strings.ToLower(b.ProjectName+"/"+b.Name)]; w != "" {
			note = " (worker " + w + ")"
		}
		fmt.Fprintf(out, "  %d) %s/%s  %s, %s%s\n", i+1, b.ProjectName, b.Name, nonEmptyStr(b.PoolName, "-"), nonEmptyStr(b.PowerState, "?"), note)
	}
	fmt.Fprint(out, "Number: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return devbox.DevBox{}, errors.New("no Dev Box chosen")
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(boxes) {
		return devbox.DevBox{}, fmt.Errorf("no Dev Box chosen (want 1-%d)", len(boxes))
	}
	return boxes[n-1], nil
}

func devBoxNames(boxes []devbox.DevBox) string {
	names := make([]string, 0, len(boxes))
	for _, b := range boxes {
		names = append(names, b.ProjectName+"/"+b.Name)
	}
	return strings.Join(names, ", ")
}

// rdpDesktop forwards a local port to the worker's Remote Desktop port and
// opens it in the platform's Remote Desktop client; it returns when the
// client exits (Windows) or on Ctrl+C.
func rdpDesktop(ctx context.Context, conn *client.Conn, worker string, port int, printOnly bool) error {
	if port <= 0 || port > 65535 {
		port = 3389
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	target := strconv.Itoa(port)
	addr := ln.Addr().String()
	unreg := registerForward(worker, target, addr)
	defer unreg()
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	acceptDone := make(chan struct{})
	defer func() { // stop accepting, then wait for the open connections
		cancel()
		<-acceptDone
		wg.Wait()
	}()
	go func() {
		defer close(acceptDone)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := forwardConn(ctx, conn, target, c); err != nil {
					fmt.Fprintf(os.Stderr, "longarm: desktop %s: %s\n", worker, oneLine(err))
				}
			}()
		}
	}()
	fmt.Fprintf(os.Stderr, "longarm: Remote Desktop of %s on %s (Ctrl+C to stop)\n", worker, addr)
	if printOnly {
		fmt.Println(addr)
		<-ctx.Done()
		return nil
	}
	rdp, err := writeRDPFile(worker, addr)
	if err != nil {
		return err
	}
	defer os.Remove(rdp)
	wait, err := launchRDP(rdp, addr)
	if err != nil {
		return err
	}
	if wait == nil { // the client runs on its own: keep forwarding until Ctrl+C
		<-ctx.Done()
		return nil
	}
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

// rdpFileContent is a minimal .rdp file for addr.
func rdpFileContent(addr string) string {
	return "full address:s:" + addr + "\r\nprompt for credentials:i:1\r\nadministrative session:i:0\r\n"
}

func writeRDPFile(worker, addr string) (string, error) {
	f, err := os.CreateTemp("", "longarm-"+sanitizeFileName(worker)+"-*.rdp")
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(rdpFileContent(addr))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func sanitizeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}
