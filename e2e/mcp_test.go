package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCP drives `longarm mcp` with the official Go MCP client over stdio
// against a hub and a worker: every tool round-trips.
func TestMCP(t *testing.T) {
	t.Parallel()
	h := newHubEnv(t, "")
	os.Remove(filepath.Join(h.client, "workers.toml"))
	wa := h.newWorkerDir(t, "wa", "")
	h.joinWorker(t, wa, true)
	wa.start(t, true)
	h.waitNode(t, "wa", "online with metrics", func(n map[string]any) bool { return isTrue(n["online"]) && n["metrics"] != nil })
	t.Cleanup(func() { h.longarm(h.client, "daemon", "stop") })

	cmd := exec.Command(bins.longarm, "mcp")
	cmd.Env = append(os.Environ(), "LONGARM_CONFIG_DIR="+h.client, "MCP_SECRET=s3cr3t-value-123", "LONGARM_MCP_SECRETS=MCP_SECRET")
	cmd.Stderr = os.Stderr
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cl := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	sess, err := cl.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	want := "fleet_exec fleet_forward fleet_job_kill fleet_job_logs fleet_job_submit fleet_job_wait fleet_link fleet_read fleet_status fleet_sync fleet_write"
	if strings.Join(names, " ") != want {
		t.Fatalf("tools: %v", names)
	}

	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.IsError {
			var msg []string
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					msg = append(msg, tc.Text)
				}
			}
			t.Fatalf("%s error: %s", name, strings.Join(msg, "; "))
		}
		b, _ := json.Marshal(res.StructuredContent)
		var out map[string]any
		json.Unmarshal(b, &out)
		return out
	}

	st := call("fleet_status", nil)
	if st["source"] != "hub" || !strings.Contains(fmt.Sprint(st["workers"]), "name:wa") {
		t.Fatalf("status %v", st)
	}

	shellCmd := "echo hi-from-mcp; exit 3"
	if runtime.GOOS == "windows" {
		shellCmd = "Write-Output hi-from-mcp; exit 3"
	}
	r := call("fleet_exec", map[string]any{"command": shellCmd})
	if fmt.Sprint(r["exit_code"]) != "3" || !strings.Contains(fmt.Sprint(r["output"]), "hi-from-mcp") || r["worker"] != "wa" {
		t.Fatalf("exec %v", r)
	}
	// Large output: head + tail + a job handle; the rest via fleet_job_logs.
	r = call("fleet_exec", map[string]any{"worker": "wa", "argv": []string{bins.helper, "print", "6000", "0", "0"}, "max_output_bytes": 4096})
	out := fmt.Sprint(r["output"])
	if r["truncated"] != true || !strings.Contains(out, "line 1\n") || !strings.Contains(out, "line 6000") || !strings.Contains(out, "bytes omitted") || len(out) > 5000 {
		t.Fatalf("truncated exec: %v (len %d)", r["truncated"], len(out))
	}
	l := call("fleet_job_logs", map[string]any{"job_id": r["job_id"], "offset": 0, "max_bytes": 100})
	if !strings.HasPrefix(fmt.Sprint(l["text"]), "line 1\nline 2") || fmt.Sprint(l["next_offset"]) != "100" {
		t.Fatalf("logs %v", l)
	}
	// Secrets by name from longarm mcp's environment; redacted in the output.
	r = call("fleet_exec", map[string]any{"worker": "wa", "argv": []string{bins.helper, "env", "MCP_SECRET"}, "secret_env": []string{"MCP_SECRET"}})
	if o := fmt.Sprint(r["output"]); !strings.Contains(o, "MCP_SECRET=***") || strings.Contains(o, "s3cr3t") {
		t.Fatalf("secret %v", r)
	}
	// Variables the operator did not allow cannot be pulled out.
	if res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "fleet_exec", Arguments: map[string]any{"worker": "wa",
		"argv": []string{bins.helper, "env", "PATH"}, "secret_env": []string{"PATH"}}}); err != nil || !res.IsError {
		t.Fatalf("non-allowlisted secret accepted: %v %v", res, err)
	}

	// Jobs: submit (auto placement) / wait / kill.
	j := call("fleet_job_submit", map[string]any{"argv": []string{bins.helper, "print", "3", "100", "0"}, "name": "mcp-job"})
	w := call("fleet_job_wait", map[string]any{"job_id": j["job_id"], "timeout_seconds": 60})
	if w["state"] != "done" || fmt.Sprint(w["exit_code"]) != "0" || !strings.Contains(fmt.Sprint(w["output"]), "line 3") {
		t.Fatalf("wait %v", w)
	}
	k := call("fleet_job_submit", map[string]any{"worker": "wa", "argv": []string{bins.helper, "sleep", "60"}})
	if w := call("fleet_job_wait", map[string]any{"job_id": k["job_id"], "timeout_seconds": 1}); w["state"] != "running" && w["state"] != "preparing" { // still going (under load it may not have started yet)
		t.Fatalf("short wait %v", w)
	}
	if kr := call("fleet_job_kill", map[string]any{"worker": "wa", "job_id": k["job_id"]}); kr["state"] != "killed" {
		t.Fatalf("kill %v", kr)
	}

	// Files.
	remote := filepath.ToSlash(filepath.Join(h.root, "mcp-files", "sub", "a.txt"))
	call("fleet_write", map[string]any{"worker": "wa", "path": remote, "content": "héllo\nworld\n"})
	rd := call("fleet_read", map[string]any{"worker": "wa", "path": remote})
	if rd["content"] != "héllo\nworld\n" || rd["encoding"] != "text" {
		t.Fatalf("read %v", rd)
	}
	bin := []byte{0, 1, 2, 255, 254}
	call("fleet_write", map[string]any{"worker": "wa", "path": remote + ".bin", "content": base64.StdEncoding.EncodeToString(bin), "encoding": "base64"})
	if rd := call("fleet_read", map[string]any{"worker": "wa", "path": remote + ".bin"}); rd["encoding"] != "base64" || rd["content"] != base64.StdEncoding.EncodeToString(bin) {
		t.Fatalf("binary read %v", rd)
	}

	// Sync a local checkout.
	repo := filepath.Join(h.root, "mcp-repo")
	os.MkdirAll(repo, 0o755)
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@e"}, args...)...)
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
	}
	git("init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "README.md"), []byte("committed\n"), 0o644)
	git("add", ".")
	git("commit", "-q", "-m", "c1")
	os.WriteFile(filepath.Join(repo, "new.txt"), []byte("untracked\n"), 0o644)
	dst := filepath.ToSlash(filepath.Join(h.root, "mcp-sync"))
	sy := call("fleet_sync", map[string]any{"dir": repo, "worker": "wa", "path": dst})
	if sy["commit"] == "" || fmt.Sprint(sy["bundle_bytes"]) == "0" {
		t.Fatalf("sync %v", sy)
	}
	if rd := call("fleet_read", map[string]any{"worker": "wa", "path": dst + "/new.txt"}); rd["content"] != "untracked\n" {
		t.Fatalf("synced file %v", rd)
	}

	// Forward a worker port through the longarm daemon.
	portFile := filepath.Join(h.root, "echo.port")
	echo := strings.TrimSpace(h.mustLongarm(h.client, "run", "wa", "--", bins.helper, "echo-server", portFile))
	var port string
	waitFor(t, "echo server", 20*time.Second, func() bool { b, _ := os.ReadFile(portFile); port = string(b); return port != "" })
	f := call("fleet_forward", map[string]any{"worker": "wa", "port": port})
	fw := f["forwards"].([]any)[0].(map[string]any)
	c, err := net.Dial("tcp", fmt.Sprint(fw["local_addr"]))
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("via-mcp"))
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(15 * time.Second))
	got, _ := io.ReadAll(c)
	c.Close()
	if string(got) != "via-mcp7 bytes\n" {
		t.Fatalf("through the forward: %q", got)
	}
	if ls := h.mustLongarm(h.client, "forward", "--list"); !strings.Contains(ls, "daemon") || !strings.Contains(ls, port) {
		t.Fatalf("forward --list: %s", ls)
	}
	if s := call("fleet_forward", map[string]any{"action": "stop"}); len(s["forwards"].([]any)) != 1 {
		t.Fatalf("stop %v", s)
	}
	h.mustLongarm(h.client, "kill", echo)

	// Port links: list (none yet); a link needs two workers - refused for one.
	if l := call("fleet_link", map[string]any{"action": "list"}); len(l["links"].([]any)) != 0 {
		t.Fatalf("links %v", l)
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "fleet_link", Arguments: map[string]any{"on": "wa", "target": "wa:8080"}})
	if err != nil || !res.IsError || !strings.Contains(fmt.Sprint(res.Content[0]), "same worker") {
		t.Fatalf("fleet_link to itself: %v %+v", err, res)
	}
}
