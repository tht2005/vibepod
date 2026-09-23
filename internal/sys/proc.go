package sys

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
)

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
