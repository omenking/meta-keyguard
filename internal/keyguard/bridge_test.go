package keyguard

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// The test binary doubles as `meta-keyguard` when run with KEYGUARD_TEST_MAIN set, so the bridge and the
// CLI can be tested as real processes talking over pipes and the Unix socket.
func TestMain(m *testing.M) {
	if os.Getenv("KEYGUARD_TEST_MAIN") == "1" {
		os.Exit(Main(os.Args[1:], fstest.MapFS{}))
	}
	os.Exit(m.Run())
}

func keyguardCommand(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "KEYGUARD_TEST_MAIN=1")
	return cmd
}

// fakeChrome plays the extension's side of native messaging against a real `meta-keyguard bridge` process.
type fakeChrome struct {
	t        *testing.T
	meta     *fakeMeta
	process  *exec.Cmd
	stdin    io.WriteCloser
	mu       sync.Mutex
	received []map[string]any
	done     chan struct{}
	closed   bool
}

func startChrome(t *testing.T) *fakeChrome {
	t.Helper()
	c := &fakeChrome{t: t, meta: newFakeMeta(), process: keyguardCommand("bridge"), done: make(chan struct{})}
	stdin, err := c.process.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := c.process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.stdin = stdin
	if err := c.process.Start(); err != nil {
		t.Fatal(err)
	}
	c.send(map[string]any{"type": "hello", "version": "test"})
	go c.serve(stdout)
	t.Cleanup(c.close)
	path, _ := SocketPath()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge did not open its socket")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *fakeChrome) serve(stdout io.Reader) {
	defer close(c.done)
	for {
		message, err := ReadMessage(stdout)
		if err != nil {
			return
		}
		c.mu.Lock()
		c.received = append(c.received, message)
		c.mu.Unlock()
		op, ok := message["op"].(string)
		if !ok {
			continue
		}
		params, _ := message["params"].(map[string]any)
		c.mu.Lock()
		result, err := c.meta.Call(op, Params(params), time.Minute)
		c.mu.Unlock()
		reply := map[string]any{"request_id": message["request_id"], "ok": true, "result": result}
		if err != nil {
			e := err.(*ExtensionError)
			reply = map[string]any{"request_id": message["request_id"], "ok": false,
				"error": map[string]any{"code": e.Code, "message": e.Error()}}
		}
		c.send(reply)
	}
}

func (c *fakeChrome) send(message any) {
	if err := WriteMessage(c.stdin, message); err != nil {
		c.t.Logf("write to bridge: %v", err)
	}
}

func (c *fakeChrome) messages() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.received...)
}

func (c *fakeChrome) close() {
	if c.closed {
		return
	}
	c.closed = true
	c.stdin.Close()
	exited := make(chan error, 1)
	go func() { exited <- c.process.Wait() }()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		c.process.Process.Kill()
		c.t.Error("bridge did not exit when Chrome closed the pipe")
	}
}

type cliResult struct {
	code           int
	stdout, stderr string
}

func cli(t *testing.T, args ...string) cliResult {
	t.Helper()
	cmd := keyguardCommand(args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return cliResult{code, stdout.String(), stderr.String()}
}

func mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	result := cli(t, args...)
	if result.code != 0 {
		t.Fatalf("meta-keyguard %s failed (%d): %s", strings.Join(args, " "), result.code, result.stderr)
	}
	return result.stdout
}

func contains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%q does not contain %q", text, want)
	}
}

func TestCLIRoundTripThroughTheBridge(t *testing.T) {
	setup(t)
	chrome := startChrome(t)
	contains(t, mustCLI(t, "create", "dev", "--ttl", "2h"), "Created dev-kg")
	secret := strings.TrimSpace(mustCLI(t, "get", "dev"))
	if !strings.HasPrefix(secret, "sk-meta-") {
		t.Fatalf("secret %q", secret)
	}
	t.Setenv("META_API_KEY", "stale")
	equal(t, strings.TrimSpace(mustCLI(t, "exec", "dev", "--", "sh", "-c", "echo $META_API_KEY")), secret)
	contains(t, mustCLI(t, "env", "dev", "--var", "MUSE_API_KEY"), "export MUSE_API_KEY="+secret)
	listing := mustCLI(t, "list", "--all")
	contains(t, listing, "hand-made")
	contains(t, listing, "1h 59m")
	// The bridge pushed the schedule to the extension after the create.
	var last map[string]any
	for _, m := range chrome.messages() {
		if m["op"] == "schedule" {
			last = m["params"].(map[string]any)
		}
	}
	equal(t, last["keys"].([]any)[0].(map[string]any)["label"], any("dev"))
	refused := cli(t, "kill")
	equal(t, refused.code, 1)
	contains(t, refused.stderr, "--yes")
	mustCLI(t, "kill", "--yes")
	chrome.mu.Lock()
	equal(t, chrome.meta.names(), []string{SentinelName})
	chrome.mu.Unlock()
	equal(t, cli(t, "get", "dev").code, 1)
	status := mustCLI(t, "status", "--json")
	if strings.Contains(status, "sk-meta-") {
		t.Fatal("status --json contains a secret")
	}
	contains(t, status, `"bridge_running": true`)
}

func TestExtensionFailuresReachTheCLI(t *testing.T) {
	setup(t)
	chrome := startChrome(t)
	chrome.mu.Lock()
	chrome.meta.fail["create"] = "not_logged_in"
	chrome.mu.Unlock()
	done := cli(t, "create", "dev")
	equal(t, done.code, 2)
	contains(t, done.stderr, "not_logged_in")
	contains(t, done.stderr, "Sign in at https://dev.meta.ai/")
	status := cli(t, "status")
	equal(t, status.code, 1)
	contains(t, status.stdout, "Last failure")
}

func TestEventsFromTheExtensionUpdateTheStore(t *testing.T) {
	f := setup(t)
	chrome := startChrome(t)
	mustCLI(t, "create", "dev")
	k, _ := f.store.ActiveKey("dev")
	chrome.send(map[string]any{"type": "event", "event": map[string]any{"event_id": "e1", "kind": "deleted",
		"meta_id": k.MetaID, "reason": "expired (deleted by the extension)"}})
	deadline := time.Now().Add(10 * time.Second)
	for {
		acked := false
		for _, m := range chrome.messages() {
			acked = acked || m["type"] == "event_ack"
		}
		if acked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no acknowledgement")
		}
		time.Sleep(20 * time.Millisecond)
	}
	row := f.byMetaID(k.MetaID)
	equal(t, []string{row.State, row.Secret}, []string{StateRevoked, ""})
}

func TestRequestsFailFastOnceChromeDisconnects(t *testing.T) {
	setup(t)
	chrome := startChrome(t)
	chrome.close()
	done := cli(t, "list", "--all")
	equal(t, done.code, 2)
	contains(t, done.stderr, "bridge_offline")
	path, _ := SocketPath()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("the socket outlived the bridge")
	}
}

func TestHelpAndUsageErrors(t *testing.T) {
	setup(t)
	equal(t, cli(t, "create", "--help").code, 0)
	contains(t, cli(t, "--help").stdout, "install-bridge")
	contains(t, cli(t, "--version").stdout, Version)
	bad := cli(t, "create")
	equal(t, bad.code, 2)
	contains(t, bad.stderr, "usage: meta-keyguard create")
}
