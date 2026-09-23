package pod

import (
	"os"
	"os/exec"
	"strings"
)

// lookPath resolves argv[0] against the PATH the process will actually run
// with, not the one vpinit happens to have.
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	path := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "PATH="); ok {
			path = v
		}
	}
	if path == "" {
		return exec.LookPath(name)
	}
	old, had := os.LookupEnv("PATH")
	os.Setenv("PATH", path)
	defer func() {
		if had {
			os.Setenv("PATH", old)
		} else {
			os.Unsetenv("PATH")
		}
	}()
	return exec.LookPath(name)
}
