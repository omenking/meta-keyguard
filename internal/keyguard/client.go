package keyguard

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"time"
)

// Hints says what to tell the user for each failure the extension or bridge can report. Failures
// are never silent: every one reaches the CLI as an *ExtensionError carrying one of these codes.
var Hints = map[string]string{
	"bridge_offline": "Chrome is not running, or the Meta Keyguard extension is not connected. Open Chrome, " +
		"check the extension is enabled, and run `keyguard doctor`.",
	"timeout":       "The extension did not answer in time. Check that Chrome is responsive and that dev.meta.ai loads.",
	"not_logged_in": "Sign in at https://dev.meta.ai/ in Chrome, then try again.",
	"reauth_required": "dev.meta.ai wants you to confirm your identity. Open https://dev.meta.ai/api-keys in Chrome, " +
		"complete the prompt, then try again.",
	"forbidden":      "dev.meta.ai refused the request for this account.",
	"create_blocked": "dev.meta.ai will not create keys for this account (for example, no payment method).",
	"endpoint_changed": "dev.meta.ai answered in a way Keyguard does not recognise; its internal API may have " +
		"changed. Run `keyguard doctor --inspect` and update extension/adapter.js.",
	"secret_not_found": "A key was created but Keyguard could not find its secret in the response, so it deleted " +
		"that key again. The internal API has probably changed; see `keyguard doctor --inspect`.",
	"multiple_teams": "Your account has several teams. Choose one with `keyguard config set team_id <id>`.",
	"multiple_projects": "This team has several projects. Choose one with `keyguard config set project_id <id>` " +
		"(the details below list them).",
	"project_unknown": "Keyguard could not find which project to create keys in. Open https://dev.meta.ai/api-keys " +
		"in Chrome and pick the project, or set it with `keyguard config set project_id <id>`; " +
		"the project ID appears as ?project_id= in the dashboard URL.",
	"rate_limited":     "dev.meta.ai is rate limiting requests. Wait a minute and try again.",
	"meta_unavailable": "dev.meta.ai returned a server error. Try again later.",
	"network":          "Chrome could not reach dev.meta.ai.",
	"last_key":         "dev.meta.ai will not delete the last key on a team, and Keyguard could not create a sentinel key.",
	"tab_failed":       "The extension could not open or use a dev.meta.ai tab.",
}

// ExtensionError is a failure reported by the extension, or by the bridge on its behalf.
type ExtensionError struct {
	Code    string
	Message string
	Detail  any
}

func NewExtensionError(code string) *ExtensionError {
	return &ExtensionError{Code: code}
}

func (e *ExtensionError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if hint, ok := Hints[e.Code]; ok {
		return hint
	}
	return e.Code
}

func (e *ExtensionError) Hint() string { return Hints[e.Code] }

// Params are the arguments of an extension operation.
type Params map[string]any

// Extension is anything that can perform operations in the browser: the bridge itself, the
// socket client the CLI uses, or a fake in tests.
type Extension interface {
	Call(op string, params Params, timeout time.Duration) (map[string]any, error)
}

const defaultTimeout = 90 * time.Second

// replyResult turns a reply into its result, or the error it carries.
func replyResult(reply map[string]any) (map[string]any, error) {
	if ok, _ := reply["ok"].(bool); ok {
		result, _ := reply["result"].(map[string]any)
		if result == nil {
			result = map[string]any{}
		}
		return result, nil
	}
	failure, _ := reply["error"].(map[string]any)
	code, _ := failure["code"].(string)
	if code == "" {
		code = "unknown"
	}
	message, _ := failure["message"].(string)
	return nil, &ExtensionError{Code: code, Message: message, Detail: failure["detail"]}
}

// BridgeClient sends one request per connection to the running `keyguard bridge`.
type BridgeClient struct{ Path string }

func (c BridgeClient) Call(op string, params Params, timeout time.Duration) (map[string]any, error) {
	path := c.Path
	if path == "" {
		var err error
		if path, err = SocketPath(); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(path); err != nil {
		return nil, NewExtensionError("bridge_offline")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, NewExtensionError("bridge_offline")
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout + 5*time.Second))
	request := map[string]any{"op": op, "timeout": timeout.Seconds()}
	for k, v := range params {
		request[k] = v
	}
	line, _ := json.Marshal(request)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, NewExtensionError("bridge_offline")
	}
	data, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, NewExtensionError("timeout")
		}
		if len(data) == 0 {
			return nil, &ExtensionError{Code: "bridge_offline", Message: "The bridge closed the connection without answering."}
		}
	}
	var reply map[string]any
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, err
	}
	return replyResult(reply)
}
