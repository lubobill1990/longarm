//go:build windows

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"longarm/internal/screen"
)

// agent is what the Longarm Desktop service will do (run it as SYSTEM, in
// session 0): keep `spike serve` running as SYSTEM in the console session,
// starting it again when it exits or the console session changes (a user
// logs on, a remote session takes the console over). It stops after
// seconds (0: never), taking the server with it.
func agent(serveArgs []string, seconds int) {
	exe, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	}
	cmdline := syscall.EscapeArg(exe) + " serve"
	for _, a := range serveArgs {
		cmdline += " " + syscall.EscapeArg(a)
	}
	var end <-chan time.Time
	if seconds > 0 {
		end = time.After(time.Duration(seconds) * time.Second)
	}
	for {
		sid := screen.ConsoleSession()
		if sid == 0xFFFFFFFF {
			time.Sleep(time.Second)
			continue
		}
		proc, err := screen.StartInSession(sid, cmdline)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("started the server in console session %d: %s", sid, cmdline)
		for {
			select {
			case <-end:
				windows.TerminateProcess(proc, 0)
				windows.CloseHandle(proc)
				log.Print("time is up: stopped")
				return
			default:
			}
			ev, _ := windows.WaitForSingleObject(proc, 2000)
			if ev == windows.WAIT_OBJECT_0 {
				var code uint32
				windows.GetExitCodeProcess(proc, &code)
				log.Printf("the server exited (%d): again", code)
				break
			}
			if now := screen.ConsoleSession(); now != sid {
				log.Printf("console session %d -> %d: moving", sid, now)
				windows.TerminateProcess(proc, 0)
				break
			}
		}
		windows.CloseHandle(proc)
		time.Sleep(time.Second)
	}
}

// poke sends input to serve without viewing (tests of the secure
// desktop): sas, key:<scan>[,<scan>...] (pressed in order, released in
// reverse: key:0x1D,0x38,0xE053 is Ctrl+Alt+Del's keys), click:<x>,<y>,
// sleep:<ms>.
func poke(addr string, cmds []string) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	bw := bufio.NewWriter(c)
	send := func(m any) {
		b, _ := json.Marshal(m)
		writeFrame(bw, b)
		bw.Flush()
	}
	writeFrame(bw, []byte("POKE"))
	for _, cmd := range cmds {
		kind, arg, _ := strings.Cut(cmd, ":")
		switch kind {
		case "sas":
			send(inputMsg{T: "sas"})
		case "key":
			var scans []uint16
			for _, s := range strings.Split(arg, ",") {
				v, err := strconv.ParseUint(s, 0, 16)
				if err != nil {
					log.Fatalf("key %q: %v", s, err)
				}
				scans = append(scans, uint16(v))
			}
			for _, s := range scans {
				send(inputMsg{T: "k", C: s, D: true})
			}
			for i := len(scans) - 1; i >= 0; i-- {
				send(inputMsg{T: "k", C: scans[i], D: false})
			}
		case "click":
			var x, y int
			if _, err := fmt.Sscanf(arg, "%d,%d", &x, &y); err != nil {
				log.Fatalf("click %q: %v", arg, err)
			}
			send(inputMsg{T: "m", X: x, Y: y})
			send(inputMsg{T: "b", B: screen.ButtonLeft, D: true})
			send(inputMsg{T: "b", B: screen.ButtonLeft, D: false})
		case "sleep":
			ms, _ := strconv.Atoi(arg)
			bw.Flush()
			time.Sleep(time.Duration(ms) * time.Millisecond)
		default:
			log.Fatalf("unknown poke %q", cmd)
		}
	}
	bw.Flush()
	time.Sleep(300 * time.Millisecond) // let the server read it all
}
