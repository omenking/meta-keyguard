package keyguard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Unix socket paths are limited to 108 bytes on Linux, including the terminating NUL.
const maxSocketPath = 100

// DataDir is where Keyguard keeps its state. The native host is launched by Chrome without
// the user's shell environment, so the installer passes it explicitly as META_KEYGUARD_DATA.
func DataDir() string {
	if explicit := os.Getenv("META_KEYGUARD_DATA"); explicit != "" {
		return expandHome(explicit)
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "meta-keyguard")
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

func EnsureDataDir() (string, error) {
	dir := DataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, os.Chmod(dir, 0o700)
}

func DBPath() string   { return filepath.Join(DataDir(), "keys.sqlite3") }
func LockPath() string { return filepath.Join(DataDir(), "ops.lock") }
func LogPath() string  { return filepath.Join(DataDir(), "bridge.log") }

func SocketPath() (string, error) {
	preferred := filepath.Join(DataDir(), "bridge.sock")
	if len(preferred) <= maxSocketPath {
		return preferred, nil
	}
	// A deep data directory would overflow the limit, so use a short private directory keyed by it.
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = fmt.Sprintf("/tmp/meta-keyguard-%d", os.Getuid())
		if err := os.MkdirAll(runtime, 0o700); err != nil {
			return "", err
		}
		info, err := os.Lstat(runtime)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByMe(info) {
			return "", fmt.Errorf("%s is not a private directory owned by you", runtime)
		}
	}
	sum := sha256.Sum256([]byte(DataDir()))
	return filepath.Join(runtime, "meta-keyguard-"+hex.EncodeToString(sum[:])[:16]+".sock"), nil
}
