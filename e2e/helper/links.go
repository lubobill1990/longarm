package main

// Workloads for the port link tests (e2e/links_test.go).

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// addrArg resolves "$VAR" to the environment variable's value.
func addrArg(s string) string {
	if v, ok := strings.CutPrefix(s, "$"); ok {
		return os.Getenv(v)
	}
	return s
}

func httpServe(secs string) {
	n, _ := strconv.Atoi(secs)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("port %d\n", ln.Addr().(*net.TCPAddr).Port)
	who := "?"
	if u, err := user.Current(); err == nil {
		who = u.Username
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s from %s", r.URL.Path, who)
	}))
	time.Sleep(time.Duration(n) * time.Second)
}

func httpGet(addr, path string) {
	if addr == "" {
		fmt.Fprintln(os.Stderr, "no address")
		os.Exit(1)
	}
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get("http://" + addr + "/" + strings.TrimPrefix(path, "/"))
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("status %d: %s\n", resp.StatusCode, b)
}

func hold(addr string) {
	c, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		fmt.Println("cut:", err)
		os.Exit(1)
	}
	defer c.Close()
	r := bufio.NewReader(c)
	up := false
	for i := 0; ; i++ {
		c.SetDeadline(time.Now().Add(20 * time.Second))
		if _, err := fmt.Fprintf(c, "ping %d\n", i); err != nil {
			fmt.Println("cut:", err)
			return
		}
		line, err := r.ReadString('\n')
		if err != nil {
			fmt.Println("cut:", err)
			return
		}
		if !strings.HasPrefix(line, "ping") {
			fmt.Println("cut: bad echo", line)
			return
		}
		if !up {
			up = true
			fmt.Println("up")
		}
		if i > 0 && i%25 == 0 { // progress: the connection still carries traffic
			fmt.Printf("pings %d\n", i)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
