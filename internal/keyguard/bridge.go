package keyguard

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The bridge is the native messaging host. Chrome starts it through wsl.exe and keeps its stdin and
// stdout open; the bridge relays requests from CLI processes (over a Unix socket) to the extension
// (over native messaging), applies events the extension reports on its own, and runs the reaper once
// a minute for as long as Chrome keeps it alive.

// Version is set at release time with -ldflags "-X .../internal/keyguard.Version=<version>".
var Version = "0.1.0-dev"

func init() {
	// `go install ...@v1.2.3` records the module version, which is better than the placeholder.
	if info, ok := debug.ReadBuildInfo(); ok && strings.HasSuffix(Version, "-dev") &&
		info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = strings.TrimPrefix(info.Main.Version, "v")
	}
}

var reapInterval = 60 * time.Second

type Bridge struct {
	in       io.Reader
	out      io.Writer
	sockPath string
	store    *Store
	ops      *Ops

	writeMu          sync.Mutex
	mu               sync.Mutex
	pending          map[string]chan map[string]any
	stopping         chan struct{}
	stopOnce         sync.Once
	extensionVersion any
	startedAt        time.Time
	listener         net.Listener
	sockInode        uint64
}

func NewBridge(in io.Reader, out io.Writer, sockPath string, store *Store) *Bridge {
	b := &Bridge{in: in, out: out, sockPath: sockPath, store: store, pending: map[string]chan map[string]any{},
		stopping: make(chan struct{}), startedAt: time.Now()}
	b.ops = &Ops{Store: store, Ext: b}
	return b
}

func (b *Bridge) stop() { b.stopOnce.Do(func() { close(b.stopping) }) }

func (b *Bridge) stopped() bool {
	select {
	case <-b.stopping:
		return true
	default:
		return false
	}
}

func (b *Bridge) send(message any) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	return WriteMessage(b.out, message)
}

// Call asks the extension to do something and waits for its answer.
func (b *Bridge) Call(op string, params Params, timeout time.Duration) (map[string]any, error) {
	if b.stopped() {
		return nil, NewExtensionError("bridge_offline")
	}
	id := make([]byte, 16)
	rand.Read(id)
	requestID := hex.EncodeToString(id)
	slot := make(chan map[string]any, 1)
	b.mu.Lock()
	b.pending[requestID] = slot
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, requestID)
		b.mu.Unlock()
	}()
	if params == nil {
		params = Params{}
	}
	if err := b.send(map[string]any{"request_id": requestID, "op": op, "params": params}); err != nil {
		return nil, &ExtensionError{Code: "bridge_offline", Message: "Could not write to Chrome: " + err.Error()}
	}
	select {
	case reply := <-slot:
		return replyResult(reply)
	case <-time.After(timeout):
		return nil, NewExtensionError("timeout")
	}
}

func (b *Bridge) readLoop() {
	defer func() {
		b.stop()
		// Chrome has gone, so nothing will answer the requests still waiting.
		b.mu.Lock()
		for _, slot := range b.pending {
			select {
			case slot <- map[string]any{"ok": false, "error": map[string]any{"code": "bridge_offline"}}:
			default:
			}
		}
		b.mu.Unlock()
	}()
	for !b.stopped() {
		message, err := ReadMessage(b.in)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			log.Printf("unreadable message from the extension: %v", err)
			continue
		}
		if requestID, ok := message["request_id"].(string); ok {
			b.mu.Lock()
			slot := b.pending[requestID]
			b.mu.Unlock()
			if slot != nil {
				slot <- message
			}
			continue
		}
		switch message["type"] {
		case "hello":
			b.mu.Lock()
			b.extensionVersion = message["version"]
			b.mu.Unlock()
			// A (re)connected extension gets the current schedule straight away.
			go b.ops.PushSchedule()
		case "event":
			event, _ := message["event"].(map[string]any)
			b.applyEvent(event)
		}
	}
}

// applyEvent records what the extension did while acting on its own, then acknowledges it.
func (b *Bridge) applyEvent(event map[string]any) {
	str := func(key string) string { s, _ := event[key].(string); return s }
	metaID := str("meta_id")
	switch str("kind") {
	case "deleted":
		if k, _ := b.store.ByMetaID(metaID); k != nil && k.State != StateRevoked {
			reason := str("reason")
			if reason == "" {
				reason = "deleted by the extension"
			}
			b.store.MarkRevoked(*k, reason)
		}
	case "sentinel-created":
		if k, _ := b.store.ByMetaID(metaID); k == nil {
			at := str("at")
			if at == "" {
				at = ISO(now())
			}
			b.store.InsertKey(Key{Label: SentinelName, MetaID: metaID, MetaName: str("name"), State: StateSentinel, CreatedAt: at})
		}
	case "failure":
		op, code := str("op"), str("code")
		if op == "" {
			op = "extension"
		}
		if code == "" {
			code = "unknown"
		}
		b.store.RecordFailure(op, code, str("message"))
		if metaID != "" {
			if k, _ := b.store.ByMetaID(metaID); k != nil {
				b.store.UpdateKey(k.ID, map[string]string{"last_error": code + ": " + str("message")})
			}
		}
	}
	if id := str("event_id"); id != "" {
		b.send(map[string]any{"type": "event_ack", "event_id": id})
	}
}

func (b *Bridge) listen() error {
	if _, err := EnsureDataDir(); err != nil {
		return err
	}
	os.Remove(b.sockPath)
	old := syscall.Umask(0o177)
	listener, err := net.Listen("unix", b.sockPath)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if info, err := os.Stat(b.sockPath); err == nil {
		b.sockInode = info.Sys().(*syscall.Stat_t).Ino
	}
	b.listener = listener
	return nil
}

func (b *Bridge) serve() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			return
		}
		go b.handleClient(conn)
	}
}

func (b *Bridge) handleClient(conn net.Conn) {
	defer conn.Close()
	data, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var request map[string]any
	if err := json.Unmarshal(data, &request); err != nil {
		return
	}
	op, _ := request["op"].(string)
	timeout := defaultTimeout
	if seconds, ok := request["timeout"].(float64); ok && seconds > 0 {
		timeout = time.Duration(seconds * float64(time.Second))
	}
	delete(request, "op")
	delete(request, "timeout")
	var result map[string]any
	if op == "bridge-info" {
		b.mu.Lock()
		result = map[string]any{"bridge_version": Version, "extension_version": b.extensionVersion,
			"pid": os.Getpid(), "started_at": float64(b.startedAt.UnixNano()) / 1e9}
		b.mu.Unlock()
	} else {
		result, err = b.Call(op, Params(request), timeout)
	}
	reply := map[string]any{"ok": true, "result": result}
	if err != nil {
		var extErr *ExtensionError
		if !errors.As(err, &extErr) {
			extErr = &ExtensionError{Code: "unknown", Message: err.Error()}
		}
		reply = map[string]any{"ok": false, "error": map[string]any{"code": extErr.Code, "message": extErr.Error(), "detail": extErr.Detail}}
	}
	line, _ := json.Marshal(reply)
	conn.Write(append(line, '\n'))
}

func (b *Bridge) reapLoop() {
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-b.stopping:
			return
		case <-ticker.C:
			b.reapOnce()
		}
	}
}

func (b *Bridge) reapOnce() {
	unlock, err := Lock()
	if err != nil {
		log.Printf("reaper could not take the lock: %v", err)
		return
	}
	defer unlock()
	// Failures are already recorded by Ops.Call; the loop carries on regardless.
	report, err := b.ops.Reap(now())
	if err != nil {
		log.Printf("reaper: %v", err)
	}
	for _, f := range report.Failures {
		log.Printf("reaper: %s: %v", f.Name, f.Err)
	}
}

// Run serves until Chrome closes the native messaging pipe.
func (b *Bridge) Run() error {
	if err := b.listen(); err != nil {
		return err
	}
	go b.serve()
	go b.reapLoop()
	b.readLoop()
	b.listener.Close()
	// A second Chrome profile may have started a newer bridge on the same path; leave it alone.
	if info, err := os.Stat(b.sockPath); err == nil && info.Sys().(*syscall.Stat_t).Ino == b.sockInode {
		os.Remove(b.sockPath)
	}
	return nil
}

// RunBridge is `meta-keyguard bridge`. Anything printed to stdout would corrupt the native messaging
// stream, so diagnostics go to a log file in the data directory (Chrome discards the host's stderr).
func RunBridge() error {
	if _, err := EnsureDataDir(); err != nil {
		return err
	}
	logFile, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	log.SetOutput(logFile)
	os.Stderr = logFile
	store, err := OpenStore()
	if err != nil {
		log.Printf("could not open the store: %v", err)
		return err
	}
	defer store.Close()
	// Chrome starts the bridge with a bare PATH, so restore the one install-bridge recorded; Cost
	// Center and the interpreters its launcher needs are found through it.
	if saved := store.Setting("bridge_path"); saved != "" {
		os.Setenv("PATH", saved)
	}
	sockPath, err := SocketPath()
	if err != nil {
		log.Printf("no socket path: %v", err)
		return err
	}
	log.Printf("bridge %s started (pid %d)", Version, os.Getpid())
	err = NewBridge(os.Stdin, os.Stdout, sockPath, store).Run()
	if err != nil {
		log.Printf("bridge stopped: %v", err)
	}
	return err
}
