//go:build linux

package screen

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/shm"
	"github.com/jezek/xgb/xproto"
	"golang.org/x/sys/unix"
)

// Linux: X11 through jezek/xgb (the X protocol in pure Go): capture with
// MIT-SHM or GetImage and DAMAGE, input with XTEST, the viewer as a plain
// X window. Wayland sessions are not supported yet (docs/remote-desktop.md
// "macOS and Linux").

func init() { xgb.Logger = log.New(io.Discard, "", 0) } // it prints to stderr

// wayland: the session is Wayland (XWayland's root window would not show it).
func wayland() bool {
	return os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("XDG_SESSION_TYPE") == "wayland"
}

// xConnect connects to $DISPLAY. longarmd usually runs without one (a
// systemd user service): then it tries :0, with the display manager's
// Xauthority file when $XAUTHORITY is not set either.
func xConnect() (*xgb.Conn, error) {
	if wayland() {
		return nil, errors.New("Wayland sessions are not supported yet")
	}
	if os.Getenv("DISPLAY") != "" {
		c, err := xgb.NewConn()
		if err != nil {
			return nil, fmt.Errorf("X display %s: %w", os.Getenv("DISPLAY"), err)
		}
		return c, nil
	}
	if os.Getenv("XAUTHORITY") == "" {
		for _, p := range []string{
			filepath.Join("/run/user", fmt.Sprint(os.Getuid()), "gdm/Xauthority"),
			filepath.Join(os.Getenv("HOME"), ".Xauthority"),
		} {
			if _, err := os.Stat(p); err == nil {
				os.Setenv("XAUTHORITY", p) // xgb reads it from the environment
				break
			}
		}
	}
	c, err := xgb.NewConnDisplay(":0")
	if err != nil {
		return nil, fmt.Errorf("no X display ($DISPLAY is not set, and :0: %v)", err)
	}
	return c, nil
}

// xFormat checks the screen is 24/32-bit true colour with 32-bit pixels in
// BGRX order (every X server today): images then copy as they are.
func xFormat(c *xgb.Conn, s *xproto.ScreenInfo) error {
	setup := xproto.Setup(c)
	if setup.ImageByteOrder != xproto.ImageOrderLSBFirst {
		return errors.New("X server with big-endian images")
	}
	if s.RootDepth != 24 && s.RootDepth != 32 {
		return fmt.Errorf("X screen of depth %d (24 or 32 needed)", s.RootDepth)
	}
	for _, f := range setup.PixmapFormats {
		if f.Depth == s.RootDepth && f.BitsPerPixel != 32 {
			return fmt.Errorf("X pixmaps of %d bits per pixel (32 needed)", f.BitsPerPixel)
		}
	}
	for _, d := range s.AllowedDepths {
		for _, v := range d.Visuals {
			if v.VisualId == s.RootVisual && (v.RedMask != 0xFF0000 || v.GreenMask != 0xFF00 || v.BlueMask != 0xFF) {
				return errors.New("X visual is not BGRX")
			}
		}
	}
	return nil
}

func available() string {
	c, err := xConnect()
	if err != nil {
		return err.Error()
	}
	defer c.Close()
	if err := xFormat(c, xproto.Setup(c).DefaultScreen(c)); err != nil {
		return err.Error()
	}
	return ""
}

// Displays lists the monitors (RandR's, else the whole screen).
func Displays() ([]Display, error) {
	c, err := xConnect()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return xDisplays(c), nil
}

// xDisplays: the primary first, as on Windows (display 0 is the primary).
func xDisplays(c *xgb.Conn) []Display {
	s := xproto.Setup(c).DefaultScreen(c)
	whole := []Display{{Name: "screen", W: int(s.WidthInPixels), H: int(s.HeightInPixels)}}
	if randr.Init(c) != nil {
		return whole
	}
	if v, err := randr.QueryVersion(c, 1, 5).Reply(); err != nil || v.MajorVersion < 1 || (v.MajorVersion == 1 && v.MinorVersion < 5) {
		return whole
	}
	r, err := randr.GetMonitors(c, s.Root, true).Reply()
	if err != nil || len(r.Monitors) == 0 {
		return whole
	}
	var out []Display
	for _, m := range r.Monitors {
		d := Display{X: int(m.X), Y: int(m.Y), W: int(m.Width), H: int(m.Height)}
		if n, err := xproto.GetAtomName(c, m.Name).Reply(); err == nil {
			d.Name = n.Name
		}
		if m.Primary {
			out = append([]Display{d}, out...)
		} else {
			out = append(out, d)
		}
	}
	for i := range out {
		out[i].Index = i
	}
	return out
}

// xAtom interns an atom (0 on error).
func xAtom(c *xgb.Conn, name string) xproto.Atom {
	r, err := xproto.InternAtom(c, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0
	}
	return r.Atom
}

// xShm makes a shared memory segment of size bytes the X server has
// attached too (seg 0: no MIT-SHM, or a remote display). Detach both
// sides when done (the segment is freed then).
func xShm(c *xgb.Conn, size int) (shm.Seg, []byte) {
	if shm.Init(c) != nil {
		return 0, nil
	}
	if _, err := shm.QueryVersion(c).Reply(); err != nil {
		return 0, nil
	}
	id, err := unix.SysvShmGet(unix.IPC_PRIVATE, size, unix.IPC_CREAT|0o600)
	if err != nil {
		return 0, nil
	}
	mem, err := unix.SysvShmAttach(id, 0, 0)
	if err != nil {
		unix.SysvShmCtl(id, unix.IPC_RMID, nil)
		return 0, nil
	}
	seg, err := c.NewId()
	if err == nil {
		err = shm.AttachChecked(c, shm.Seg(seg), uint32(id), false).Check()
	}
	// Removed now, freed once both sides detach (also if we crash).
	unix.SysvShmCtl(id, unix.IPC_RMID, nil)
	if err != nil {
		unix.SysvShmDetach(mem)
		return 0, nil
	}
	return shm.Seg(seg), mem
}
