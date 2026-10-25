// Command helper is a portable test workload for the end-to-end tests.
//
//	helper print N INTERVAL_MS EXIT    print "line i" N times, then exit
//	helper env NAME                    print "NAME=<value>"
//	helper sleep SECONDS
//	helper cat FILE
//	helper repl                        interactive: echo lines, "size", "env X", "exit N"
//	helper echo-server PORTFILE        TCP echo server on 127.0.0.1 (port written to PORTFILE)
//	helper tree PIDFILE                spawn 2 children, each with 1 grandchild;
//	                                   every process appends its pid to PIDFILE
//	                                   and sleeps (5 processes in total)
//	az ...                             (the binary copied to az[.exe]) a fake Azure CLI, fakeaz.go
//	helper restore|replicate ...       a fake litestream (HELPER_LITESTREAM_LOG: args are
//	                                   appended there; restore exits HELPER_LITESTREAM_RESTORE_EXIT;
//	                                   replicate runs the -exec command (pid in <log>.child)
//	                                   and exits with it)
//	helper whoami                      print the account the process runs as
//	helper write FILE TEXT             write TEXT to FILE
//	helper ls DIR                      list DIR (exit 1 on error)
//	helper procenv PID                 read another process's environment
//	                                   (Windows: open it for memory reading)
//	helper serve SECONDS               TCP echo server on 127.0.0.1:0; prints
//	                                   "port N", serves for SECONDS
//	helper connect PORT                connect to 127.0.0.1:PORT, send "hi", print the answer
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"os/user"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

func main() {
	if strings.HasPrefix(strings.ToLower(filepath.Base(os.Args[0])), "az") {
		fakeAz(os.Args[1:]) // copied to az / az.exe (fakeaz.go)
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: helper print|env|sleep|tree ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "print":
		n, _ := strconv.Atoi(os.Args[2])
		ms, _ := strconv.Atoi(os.Args[3])
		code, _ := strconv.Atoi(os.Args[4])
		if len(os.Args) > 5 { // optional: ms to wait before the first line
			wait, _ := strconv.Atoi(os.Args[5])
			time.Sleep(time.Duration(wait) * time.Millisecond)
		}
		for i := 1; i <= n; i++ {
			fmt.Printf("line %d\n", i)
			if i%5 == 0 {
				fmt.Fprintf(os.Stderr, "err %d\n", i)
			}
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		os.Exit(code)
	case "cat":
		b, err := os.ReadFile(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Stdout.Write(b)
	case "env":
		fmt.Printf("%s=%s\n", os.Args[2], os.Getenv(os.Args[2]))
	case "sleep":
		s, _ := strconv.Atoi(os.Args[2])
		time.Sleep(time.Duration(s) * time.Second)
	case "tree", "tree-child", "tree-leaf":
		pidfile := os.Args[2]
		f, err := os.OpenFile(pidfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintf(f, "%d\n", os.Getpid())
		f.Close()
		var kids []*exec.Cmd
		spawn := func(mode string) {
			c := exec.Command(os.Args[0], mode, pidfile)
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			if err := c.Start(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			kids = append(kids, c)
		}
		switch os.Args[1] {
		case "tree":
			spawn("tree-child")
			spawn("tree-child")
		case "tree-child":
			spawn("tree-leaf")
		}
		fmt.Printf("%s %d started %d children\n", os.Args[1], os.Getpid(), len(kids))
		time.Sleep(10 * time.Minute)
	case "repl":
		// Interactive test program for sessions: echoes lines; "size" prints
		// the terminal size; "exit N" exits with N.
		sc := bufio.NewScanner(os.Stdin)
		fmt.Println("repl ready")
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			switch {
			case line == "size":
				w, h, err := term.GetSize(int(os.Stdout.Fd()))
				fmt.Printf("size %dx%d %v\n", w, h, err)
			case strings.HasPrefix(line, "exit "):
				code, _ := strconv.Atoi(strings.TrimPrefix(line, "exit "))
				os.Exit(code)
			case strings.HasPrefix(line, "env "):
				fmt.Printf("env %s=%s\n", line[4:], os.Getenv(line[4:]))
			default:
				fmt.Printf("echo: %s\n", line)
			}
		}
	case "echo-server":
		// Listens on 127.0.0.1:0, writes the port to PORTFILE and echoes each
		// connection's bytes back; on the client's half-close it appends
		// "<n> bytes\n" and closes.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.WriteFile(os.Args[2], []byte(strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)), 0o644)
		for {
			c, err := ln.Accept()
			if err != nil {
				os.Exit(1)
			}
			go func() {
				defer c.Close()
				n, _ := io.Copy(c, c)
				fmt.Fprintf(c, "%d bytes\n", n)
			}()
		}
	case "restore", "replicate":
		fakeLitestream(os.Args[1:])
	case "whoami":
		u, err := user.Current()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("user %s home %s\n", u.Username, os.Getenv(homeVar()))
	case "write":
		if err := os.WriteFile(os.Args[2], []byte(os.Args[3]), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("written")
	case "ls":
		ents, err := os.ReadDir(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, e := range ents {
			fmt.Println(e.Name())
		}
	case "procenv":
		pid, _ := strconv.Atoi(os.Args[2])
		out, err := procEnv(pid)
		if err != nil {
			fmt.Fprintln(os.Stderr, "procenv:", err)
			os.Exit(1)
		}
		fmt.Println(out)
	case "serve":
		secs, _ := strconv.Atoi(os.Args[2])
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("port %d\n", ln.Addr().(*net.TCPAddr).Port)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					defer c.Close()
					buf := make([]byte, 64)
					n, _ := c.Read(buf)
					fmt.Fprintf(c, "echo %s\n", buf[:n])
				}()
			}
		}()
		time.Sleep(time.Duration(secs) * time.Second)
	case "connect":
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+os.Args[2], 5*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		c.Write([]byte("hi"))
		b, _ := io.ReadAll(c)
		fmt.Printf("got %s", b)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", os.Args[1])
		os.Exit(2)
	}
}

func fakeLitestream(args []string) {
	if f, err := os.OpenFile(os.Getenv("HELPER_LITESTREAM_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		fmt.Fprintln(f, strings.Join(args, "|"))
		f.Close()
	}
	if args[0] == "restore" {
		code, _ := strconv.Atoi(os.Getenv("HELPER_LITESTREAM_RESTORE_EXIT"))
		os.Exit(code)
	}
	for i, a := range args {
		if a == "-exec" && i+1 < len(args) {
			argv := shellSplit(args[i+1])
			c := exec.Command(argv[0], argv[1:]...)
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			if err := c.Start(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			// the test kills the -exec child by this pid (no process groups on Windows)
			os.WriteFile(os.Getenv("HELPER_LITESTREAM_LOG")+".child", []byte(strconv.Itoa(c.Process.Pid)), 0o644)
			if err := c.Wait(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(2)
}

// shellSplit splits on blanks and honours single quotes (enough for the
// -exec strings longarm-hub container builds).
func shellSplit(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	for _, r := range s {
		switch {
		case r == '\'':
			inQ, have = !inQ, true
		case (r == ' ' || r == '\t') && !inQ:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}
