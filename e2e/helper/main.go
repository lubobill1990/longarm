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
package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

func main() {
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
	default:
		fmt.Fprintln(os.Stderr, "unknown mode", os.Args[1])
		os.Exit(2)
	}
}
