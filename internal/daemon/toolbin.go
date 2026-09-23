package daemon

import (
	"debug/elf"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"vibepod/internal/event"
)

// toolbin: copying a missing tool to a machine that is allowed one.
//
// `vp @prod rg TODO` on a machine without ripgrep fails, and the fix a person would
// reach for is to copy the binary over. vibepod does that for machines the config
// marks `toolbin: true`, into ~/.vp/bin and nowhere else, and only for binaries that
// will actually run there: statically linked, and built for that machine's
// architecture. A dynamically linked copy would only fail differently.
//
// Everything else gets a sentence instead of a copy.

// supplyTool copies a tool to a machine when that is allowed and would work, and
// reports whether the command should be tried again.
func (s *podState) supplyTool(host, name string) bool {
	if strings.ContainsRune(name, '/') {
		return false // an absolute path names a file on that machine, not a tool
	}
	local, err := exec.LookPath(name)
	if err != nil {
		return false // nothing here to offer either
	}
	why := portable(local, s.archOf(host))
	s.mu.Lock()
	allowed := s.toolbin[host]
	s.mu.Unlock()
	switch {
	case why != "":
		s.toolNote(host, name, fmt.Sprintf("%s is not on %s, and this machine's copy "+
			"cannot be sent: %s", name, host, why))
		return false
	case !allowed:
		s.toolNote(host, name, fmt.Sprintf("%s is not on %s; `toolbin: true` for %s "+
			"would let vibepod copy this machine's (static) %s into ~/.vp/bin there",
			name, host, host, name))
		return false
	}
	if err := s.d.pool.Host(host).Push(local, name); err != nil {
		s.toolNote(host, name, fmt.Sprintf("copy %s to %s: %v", name, host, err))
		return false
	}
	s.toolNote(host, name, fmt.Sprintf("copied %s to %s:~/.vp/bin (toolbin)", name, host))
	return true
}

func (s *podState) toolNote(host, name, msg string) {
	s.d.logf("pod %s: %s", s.name, msg)
	s.d.bus.Publish(event.Event{Kind: event.KindNotice, Pod: s.name, Target: host,
		Argv: []string{name}, Detail: msg})
}

// portable reports why a binary would not run on a machine of the given
// architecture, or "" if it would.
func portable(path, arch string) string {
	f, err := elf.Open(path)
	if err != nil {
		return "it is not a native executable (a script, perhaps)"
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return "it is dynamically linked, so it needs this machine's libraries"
		}
	}
	want := map[string]elf.Machine{"x86_64": elf.EM_X86_64, "aarch64": elf.EM_AARCH64,
		"arm64": elf.EM_AARCH64}
	if m, known := want[arch]; known && f.Machine != m {
		return fmt.Sprintf("it is built for %s and that machine is %s", f.Machine, arch)
	}
	return ""
}

// archOf asks a machine what it is, once.
func (s *podState) archOf(host string) string {
	s.mu.Lock()
	a := s.arch[host]
	s.mu.Unlock()
	if a != "" {
		return a
	}
	out, err := s.d.pool.Host(host).Capture("uname -m")
	if err != nil {
		return runtime.GOARCH
	}
	a = strings.TrimSpace(out)
	s.mu.Lock()
	if s.arch == nil {
		s.arch = map[string]string{}
	}
	s.arch[host] = a
	s.mu.Unlock()
	return a
}
