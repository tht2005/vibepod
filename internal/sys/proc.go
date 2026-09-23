package sys

import (
	"bytes"
	"fmt"
	"os"
)

// ReadCString reads a NUL-terminated string out of another process's address
// space. Used on a process frozen in execve to recover the path it is about to
// run, which the notification only gives us as a pointer.
func ReadCString(pid uint32, addr uint64, max int) (string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, max)
	n, err := f.ReadAt(buf, int64(addr))
	if n == 0 {
		return "", err
	}
	if i := bytes.IndexByte(buf[:n], 0); i >= 0 {
		return string(buf[:i]), nil
	}
	return "", fmt.Errorf("no NUL within %d bytes", max)
}

// Cwd is the working directory of a pod process, as resolved inside the pod's
// mount namespace. This is what decides where a command runs.
func Cwd(pid uint32) (string, error) {
	return os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
}
