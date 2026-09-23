package sys

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
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

// ReadStringArray reads a NULL-terminated array of string pointers, such as
// execve's argv, out of another process's address space. Bounded because the
// process is frozen while we read and nothing here should be able to hang the
// exec gate.
func ReadStringArray(pid uint32, addr uint64, maxItems, maxBytes int) ([]string, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/mem", pid))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ptrs := make([]byte, 8*maxItems)
	n, _ := f.ReadAt(ptrs, int64(addr))
	var out []string
	total := 0
	for i := 0; i+8 <= n; i += 8 {
		p := binary.LittleEndian.Uint64(ptrs[i:])
		if p == 0 {
			break
		}
		buf := make([]byte, 512)
		m, _ := f.ReadAt(buf, int64(p))
		if m == 0 {
			break
		}
		j := bytes.IndexByte(buf[:m], 0)
		if j < 0 {
			j = m
		}
		total += j
		if total > maxBytes {
			out = append(out, "...")
			break
		}
		out = append(out, string(buf[:j]))
	}
	return out, nil
}

// PPID reads a process's parent, so the exec tree can be reassembled.
func PPID(pid uint32) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// The comm field can contain spaces and parentheses; everything after the
	// last ')' is positional.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return 0
	}
	fields := bytes.Fields(b[i+2:])
	if len(fields) < 2 {
		return 0
	}
	ppid, _ := strconv.Atoi(string(fields[1]))
	return ppid
}

// Alive reports whether a pid is still running.
func Alive(pid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	return err == nil
}
