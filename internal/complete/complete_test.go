package complete

import (
	"os/exec"
	"strings"
	"testing"
)

func TestZshPatchApplies(t *testing.T) {
	if err := patched(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(zshScript, "CARAPACE_BRIDGE_CONFIG_HOME") {
		t.Fatal("the capture script still reads carapace-bridge's config")
	}
}

func TestParse(t *testing.T) {
	got := Parse("checkout -- switch branches\r\ncherry\r\ncherry\r\n\r\nsrc/\n")
	want := []Candidate{{"checkout", "switch branches"}, {"cherry", ""}, {"src/", ""}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got[i], want[i])
		}
	}
}

// run completes a line with this machine's shells, the way the daemon does on a
// backend.
func run(t *testing.T, shell, line string) []Candidate {
	if _, err := exec.LookPath(shell); err != nil {
		t.Skip(shell, "is not installed")
	}
	argv, env := Command(line, "")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append([]string{"SHELL=/bin/" + shell, "PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}, env...)
	cmd.Dir = "testdata"
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", shell, err)
	}
	return Parse(string(out))
}

func words(cs []Candidate) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Word)
	}
	return out
}

func TestShells(t *testing.T) {
	for _, sh := range []string{"zsh", "bash"} {
		t.Run(sh, func(t *testing.T) {
			if got := words(run(t, sh, "ls sub")); len(got) != 1 || got[0] != "subdir/" {
				t.Errorf("files: got %q", got)
			}
			found := false
			for _, w := range words(run(t, sh, "ech")) {
				found = found || w == "echo"
			}
			if !found {
				t.Errorf("commands: echo is not offered for ech")
			}
		})
	}
}
