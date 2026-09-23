package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"vibepod/internal/config"
	"vibepod/internal/daemon"
	"vibepod/internal/fs"
	"vibepod/internal/proto"
	"vibepod/internal/remote"
	"vibepod/internal/term"
)

// cmdDoctor reports whether this machine can host a pod, and which of the
// optional pieces are present. It separates what vibepod cannot work without
// from what merely changes how well it works, because the second kind is not a
// failure and should not read like one.
func cmdDoctor() error {
	self, _ := os.Executable()
	required := []check{
		{"vpsh binary", statErr(filepath.Join(filepath.Dir(self), "vpsh"))},
		{"unprivileged user namespaces", checkUserns()},
		{"ssh", inPath("ssh")},
	}
	fmt.Println("required")
	bad := 0
	for _, c := range required {
		if c.err != nil {
			bad++
		}
		c.print()
	}

	// A daemon outlives the binary that spawned it, so after an upgrade the
	// running one is still the old one. Not a reason a pod cannot start, but
	// worth saying plainly: the symptom is behaviour that does not match the
	// binary on disk.
	if v, err := daemonVersion(); err == nil {
		fmt.Println("\ndaemon")
		if v == daemon.Version {
			(check{"build " + v, nil}).print()
		} else {
			(check{"build", fmt.Errorf("daemon is %s, this binary is %s; "+
				"`vibepod down` your pods and it restarts on next use",
				v, daemon.Version)}).print()
		}
	}

	fmt.Println("\nremote filesystems")
	backend, err := fs.Pick()
	if err != nil {
		bad++
		(check{"a mount backend", err}).print()
	} else {
		(check{"backend in use: " + backend.Name(), nil}).print()
		if backend.Name() != "rclone" {
			for _, line := range []string{
				"rclone would cache re-reads on local disk and can be told to",
				"forget them when a command finishes; sshfs cannot, so its",
				"timeouts stay short and it pays round trips instead.",
			} {
				fmt.Printf("    %-30s %s\n", "", line)
			}
		}
	}

	// Reachability is worth knowing before a pod is created rather than in the
	// middle of an agent run.
	if cfgPath, found := config.Find(cwdOr(".")); found {
		if cfg, err := config.Load(cfgPath); err == nil {
			if hosts := cfg.HostList(); len(hosts) > 0 {
				fmt.Println("\nhosts in " + short(cfgPath))
				live := term.IsTTY(os.Stdout)
				for _, h := range hosts {
					// Name the host before waiting on it: each of these can take
					// ConnectTimeout seconds, and a silent pause with several
					// hosts configured looks like a hang. Only on a terminal — in
					// a file the escape codes would be the noise.
					if live {
						fmt.Printf("  … %s", h)
					}
					err := reachable(h)
					if live {
						fmt.Print("\r\x1b[K")
					}
					(check{h, err}).print()
				}
			}
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d required check(s) failed", bad)
	}
	return nil
}

// checkUserns is the one kernel feature v2 cannot work without. v1 needed two —
// this and seccomp user notification — and dropping the exec gate dropped the
// second along with it.
func checkUserns() error {
	b, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return nil // the knob is absent on some kernels; assume available
	}
	if strings.TrimSpace(string(b)) == "0" {
		return fmt.Errorf("disabled: run `sysctl -w user.max_user_namespaces=N` " +
			"(a pod is a user namespace; there is no fallback)")
	}
	return nil
}

type check struct {
	name string
	err  error
}

func (c check) print() {
	if c.err != nil {
		fmt.Printf("  ✗ %-30s %v\n", c.name, c.err)
		return
	}
	fmt.Printf("  ✓ %-30s\n", c.name)
}

func inPath(bin string) error {
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("not installed")
	}
	return nil
}

func cwdOr(fallback string) string {
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return fallback
}

// reachable asks ssh, with the user's own config, so ProxyJump and keys are
// exercised exactly as a real command would exercise them.
func reachable(host string) error {
	args := []string{}
	if f := os.Getenv("VIBEPOD_SSH_CONFIG"); f != "" {
		args = append(args, "-F", f)
	}
	args = append(args, "-o", "BatchMode=yes",
		fmt.Sprintf("-oConnectTimeout=%d", remote.ConnectTimeout), host, "true")
	if out, err := exec.Command("ssh", args...).CombinedOutput(); err != nil {
		// Same explanation the daemon would give, so the two never disagree.
		return fmt.Errorf("%s", remote.Explain(host, string(out)))
	}
	return nil
}

func statErr(p string) error {
	_, err := os.Stat(p)
	return err
}

func daemonVersion() (string, error) {
	c, err := connect()
	if err != nil {
		return "", err
	}
	defer c.Close()
	reply, err := call(c, &proto.Msg{Op: proto.OpPs})
	if err != nil {
		return "", err
	}
	if reply.Version == "" {
		return "(before versions were reported)", nil
	}
	return reply.Version, nil
}
