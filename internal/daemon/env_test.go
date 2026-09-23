package daemon

import "strings"

import "testing"

func TestForwardEnvCarriesTheDeltaAndNothingElse(t *testing.T) {
	// A plausible session baseline: identity, and a credential of the kind the
	// project promises never leaves this machine.
	baseline := []string{
		"PATH=/vp/bin:/usr/bin", "HOME=/home/u", "SHELL=/usr/bin/zsh",
		"HF_TOKEN=secret", "VIBEPOD_POD=work", "LANG=en_US.UTF-8",
	}
	// What a shell hands a command after `PYTHONPATH=/lib VP_PROBE=hi cmd`.
	command := append([]string{}, baseline...)
	command = append(command, "PYTHONPATH=/lib", "VP_PROBE=hi", "SHLVL=2")

	keep, refused := forwardEnv(EnvPolicy{Mode: EnvDelta}, baseline, command)
	got := strings.Join(keep, " ")
	if got != "PYTHONPATH=/lib VP_PROBE=hi" {
		t.Errorf("delta = %q, want the two variables the caller set", got)
	}
	if len(refused) != 0 {
		t.Errorf("nothing identity-shaped was set by the caller, got refused=%v", refused)
	}

	// The credential was in the baseline and unchanged, so it must not cross.
	for _, kv := range keep {
		if strings.HasPrefix(kv, "HF_TOKEN") {
			t.Fatal("a baseline credential crossed to the remote")
		}
	}
}

func TestForwardEnvRefusesIdentityEvenWhenAsked(t *testing.T) {
	baseline := []string{"PATH=/usr/bin", "HOME=/home/u"}
	command := []string{"PATH=/my/bin", "HOME=/elsewhere", "GOFLAGS=-mod=mod"}

	keep, refused := forwardEnv(EnvPolicy{Mode: EnvDelta}, baseline, command)
	if strings.Join(keep, " ") != "GOFLAGS=-mod=mod" {
		t.Errorf("keep = %v, want only the non-identity variable", keep)
	}
	// Refusing silently is the failure mode this whole change is about.
	if strings.Join(refused, " ") != "HOME PATH" {
		t.Errorf("refused = %v, want both identity variables reported", refused)
	}
}

func TestForwardEnvModes(t *testing.T) {
	baseline := []string{"A=1"}
	command := []string{"A=2", "B=3", "HF_TOKEN=secret"}

	if keep, _ := forwardEnv(EnvPolicy{Mode: EnvNone}, baseline, command); keep != nil {
		t.Errorf("none forwarded %v", keep)
	}
	keep, _ := forwardEnv(EnvPolicy{Mode: EnvExplicit, Names: []string{"B", "MISSING"}},
		baseline, command)
	if strings.Join(keep, " ") != "B=3" {
		t.Errorf("explicit = %v, want just B", keep)
	}
}
