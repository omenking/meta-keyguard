package keyguard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Registering the native messaging host with Chrome on the Windows side of WSL.

const HostName = "com.metakeyguard.bridge"

// ExtensionID is fixed by the public key in extension/manifest.json, so the host manifest can name it in advance.
const ExtensionID = "admjfgnffhpgjnnbnkogihlnpeeeenio"

const registryKey = `HKCU\Software\Google\Chrome\NativeMessagingHosts\` + HostName

type InstallError struct{ Message string }

func (e *InstallError) Error() string { return e.Message }

type windowsResult struct {
	Stdout, Stderr string
	ExitCode       int
}

// windows runs a Windows program from WSL. A Windows working directory avoids UNC path warnings.
func windows(check bool, name string, args ...string) (windowsResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/mnt/c"
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := windowsResult{stdout.String(), stderr.String(), 0}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	case err != nil:
		return result, &InstallError{name + " is not available; Keyguard needs WSL with Windows interop enabled"}
	}
	if check && result.ExitCode != 0 {
		output := strings.TrimSpace(result.Stderr)
		if output == "" {
			output = strings.TrimSpace(result.Stdout)
		}
		return result, &InstallError{fmt.Sprintf("%s %s failed: %s", name, strings.Join(args, " "), output)}
	}
	return result, nil
}

func wslpath(flag, path string) (string, error) {
	out, err := exec.Command("wslpath", flag, path).Output()
	if err != nil {
		return "", &InstallError{"wslpath " + flag + " " + path + " failed: " + err.Error()}
	}
	return strings.TrimSpace(string(out)), nil
}

func installDir() (string, error) {
	result, err := windows(true, "cmd.exe", "/c", "echo %LOCALAPPDATA%")
	if err != nil {
		return "", err
	}
	local := strings.TrimSpace(result.Stdout)
	if local == "" || strings.Contains(local, "%") {
		return "", &InstallError{"Could not find %LOCALAPPDATA% on Windows"}
	}
	linux, err := wslpath("-u", local)
	if err != nil {
		return "", err
	}
	return filepath.Join(linux, "MetaKeyguard"), nil
}

// ExtensionDigest hashes the extension files, to tell whether the installed copy is current.
func ExtensionDigest(fsys fs.FS) (string, error) {
	var files []string
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "tests" {
			return fs.SkipDir
		}
		if d.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	digest := sha256.New()
	for _, path := range files {
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return "", err
		}
		digest.Write([]byte(path))
		digest.Write([]byte{0})
		digest.Write(data)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Executable is the path of the running meta-keyguard binary, which the launcher starts as the bridge.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// hostCommand is what Chrome runs: this binary inside this distro, with the data directory pinned.
func hostCommand() (string, error) {
	distro := os.Getenv("WSL_DISTRO_NAME")
	if distro == "" {
		return "", &InstallError{"WSL_DISTRO_NAME is not set; run install-bridge from inside WSL"}
	}
	// wsl.exe takes quotes around the -d value literally, so the distro name must stay bare.
	if strings.ContainsAny(distro, " \"%&|<>^") {
		return "", &InstallError{fmt.Sprintf("Unsupported characters in the WSL distro name %q", distro)}
	}
	exe, err := Executable()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`"%%SystemRoot%%\System32\wsl.exe" -d %s --exec /usr/bin/env "META_KEYGUARD_DATA=%s" "%s" bridge`,
		distro, DataDir(), exe), nil
}

// BridgePath is the user's PATH without the Windows directories WSL appends, which the bridge
// restores at startup because Chrome launches it with a bare one.
func BridgePath() string {
	var kept []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir != "" && !strings.HasPrefix(dir, "/mnt/") {
			kept = append(kept, dir)
		}
	}
	return strings.Join(kept, string(filepath.ListSeparator))
}

func copyFS(fsys fs.FS, target string) error {
	return fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dest := filepath.Join(target, filepath.FromSlash(path))
		if d.IsDir() {
			if d.Name() == "tests" {
				return fs.SkipDir
			}
			return os.MkdirAll(dest, 0o755)
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0o644)
	})
}

type Installed struct{ Directory, Extension string }

func Install(extension fs.FS, store *Store) (Installed, error) {
	command, err := hostCommand()
	if err != nil {
		return Installed{}, err
	}
	target, err := installDir()
	if err != nil {
		return Installed{}, err
	}
	extDir := filepath.Join(target, "extension")
	if err := os.RemoveAll(extDir); err != nil {
		return Installed{}, err
	}
	if err := copyFS(extension, extDir); err != nil {
		return Installed{}, err
	}
	// cmd.exe needs CRLF line endings, and @echo off keeps the batch file from writing to the
	// stdout that carries native messages.
	if err := os.WriteFile(filepath.Join(target, "keyguard-host.bat"), []byte("@echo off\r\n"+command+"\r\n"), 0o644); err != nil {
		return Installed{}, err
	}
	manifest, _ := json.MarshalIndent(map[string]any{"name": HostName, "description": "Meta Keyguard bridge to WSL",
		"path": "keyguard-host.bat", "type": "stdio", "allowed_origins": []string{"chrome-extension://" + ExtensionID + "/"}}, "", "  ")
	manifestPath := filepath.Join(target, HostName+".json")
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		return Installed{}, err
	}
	windowsManifest, err := wslpath("-w", manifestPath)
	if err != nil {
		return Installed{}, err
	}
	if _, err := windows(true, "reg.exe", "add", registryKey, "/ve", "/t", "REG_SZ", "/d", windowsManifest, "/f"); err != nil {
		return Installed{}, err
	}
	path := BridgePath()
	if err := store.SetSetting("bridge_path", &path); err != nil {
		return Installed{}, err
	}
	dir, _ := wslpath("-w", target)
	ext, _ := wslpath("-w", extDir)
	return Installed{dir, ext}, nil
}

func Uninstall() (string, error) {
	windows(false, "reg.exe", "delete", registryKey, "/f")
	target, err := installDir()
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	return wslpath("-w", target)
}

// InstallCheck is what doctor reports about the installation.
type InstallCheck struct {
	Registered         bool
	Manifest           string
	ExtensionCurrent   *bool
	HostCommandCurrent *bool
}

func CheckInstall(extension fs.FS) (InstallCheck, error) {
	var result InstallCheck
	done, err := windows(false, "reg.exe", "query", registryKey, "/ve")
	if err != nil || done.ExitCode != 0 {
		return result, err
	}
	result.Registered = true
	lines := strings.Split(strings.TrimSpace(done.Stdout), "\n")
	value := lines[len(lines)-1]
	if i := strings.Index(value, "REG_SZ"); i >= 0 {
		value = value[i+len("REG_SZ"):]
	}
	result.Manifest = strings.TrimSpace(value)
	manifest, err := wslpath("-u", result.Manifest)
	if err != nil {
		return result, err
	}
	installed := filepath.Join(filepath.Dir(manifest), "extension")
	if _, err := os.Stat(manifest); err != nil {
		return result, nil
	}
	if _, err := os.Stat(installed); err != nil {
		return result, nil
	}
	have, err := ExtensionDigest(os.DirFS(installed))
	if err != nil {
		return result, err
	}
	want, err := ExtensionDigest(extension)
	if err != nil {
		return result, err
	}
	current := have == want
	result.ExtensionCurrent = &current
	bat, _ := os.ReadFile(filepath.Join(filepath.Dir(manifest), "keyguard-host.bat"))
	command, err := hostCommand()
	if err != nil {
		return result, err
	}
	launcher := strings.Contains(string(bat), command)
	result.HostCommandCurrent = &launcher
	return result, nil
}
