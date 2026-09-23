package daemon

import (
	"sort"
	"strings"
)

// Forwarding the caller's environment to another machine.
//
// The rule: the shell stays local, the caller's *delta* crosses, and identity
// never does.
//
// A pod's environment is not neutral data. It holds the credentials the whole
// design exists to keep on one machine — that is the headline promise — and it
// describes this machine: its PATH, its home, its dynamic loader. Sending it to
// a remote would both break that promise and overwrite the remote's own
// toolchain with paths that mean nothing there.
//
// But discarding it entirely is what the bug was, and it fails silently: a
// dropped PYTHONPATH arrives as ModuleNotFoundError, which reads as a broken
// install rather than a discarded environment.
//
// So what crosses is exactly what the caller set for this command and nothing
// else: the difference between the command's environment and the session's own,
// which is `VAR=value cmd` and `export VAR=...` and nothing a user did not type.

// EnvMode is how much of the caller's environment a routed command carries.
type EnvMode string

const (
	// EnvDelta forwards only what the caller added or changed. The default.
	EnvDelta EnvMode = "delta"
	// EnvNone forwards nothing, for anyone who wants the old behaviour
	// deliberately rather than by accident.
	EnvNone EnvMode = "none"
	// EnvExplicit forwards exactly the named variables, when set.
	EnvExplicit EnvMode = "explicit"
)

// EnvPolicy is a pod's env-forwarding configuration.
type EnvPolicy struct {
	Mode  EnvMode
	Names []string
}

// identityVars describe *this* machine, not the command. Overwriting the
// remote's values with them breaks the remote's toolchain in ways that are
// worse than the bug this file exists to fix, so they never cross — even when
// the caller set them deliberately. That last case is worth saying out loud,
// because a refusal nobody mentions is how this bug got here.
var identityVars = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TMPDIR": true, "LD_LIBRARY_PATH": true, "LD_PRELOAD": true,
	"PYTHONHOME": true,
}

// identityPrefixes are families of the same thing.
var identityPrefixes = []string{"SSH_", "XDG_", "VIBEPOD_"}

// shellNoise is bookkeeping a shell maintains for itself. It shows up in the
// delta of every single command, and the caller never typed it — so unlike the
// identity vars, dropping it silently is right. Reporting it would bury the
// one refusal that means something.
var shellNoise = map[string]bool{
	"SHLVL": true, "_": true, "PWD": true, "OLDPWD": true,
	"COLUMNS": true, "LINES": true,
}

func isIdentity(name string) bool {
	if identityVars[name] {
		return true
	}
	for _, p := range identityPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// forwardEnv decides which of a command's variables cross to another machine.
// It returns the assignments to make, and the names it refused despite the
// caller having set them, which the caller should report rather than swallow.
func forwardEnv(p EnvPolicy, baseline, command []string) (keep, refused []string) {
	switch p.Mode {
	case EnvNone:
		return nil, nil
	case EnvExplicit:
		have := envMap(command)
		for _, name := range p.Names {
			if v, ok := have[name]; ok {
				keep = append(keep, name+"="+v)
			}
		}
		return keep, nil
	}

	// Delta: new, or changed from what the session started with.
	base := envMap(baseline)
	for _, kv := range command {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if was, existed := base[name]; existed && was == value {
			continue
		}
		if shellNoise[name] {
			continue
		}
		if isIdentity(name) {
			refused = append(refused, name)
			continue
		}
		keep = append(keep, name+"="+value)
	}
	sort.Strings(keep)
	sort.Strings(refused)
	return keep, refused
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok {
			m[name] = value
		}
	}
	return m
}
