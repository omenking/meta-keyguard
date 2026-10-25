package keyguard

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is a terminal. /dev/null is a character device too, so this asks
// for the terminal attributes rather than looking at the file mode.
func isTerminal(f *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}
