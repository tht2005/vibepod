// Package complete is Tab completion for `vp shell`, by the shell that will run
// the line.
//
// The machine's own shell is asked, rather than a list kept here: zsh's
// completion system, or bash-completion, answer for thousands of commands and
// already know the ones installed on that machine. So completion runs where the
// session is — in the pod, or inside the node pod on gpu03 — in the directory
// the shell is in, and it completes that machine's files and that machine's
// commands.
//
// For zsh the capture is Valodim's zsh-capture-completion, as carapace-bridge
// ships it: a zsh in a zpty whose compadd is hooked to print what it would have
// offered. For bash it is bash-completion's own loader and the function it
// registers for the command.
package complete

import (
	_ "embed"
	"fmt"
	"strings"

	zsh "github.com/carapace-sh/carapace-bridge/third_party/github.com/Valodim/zsh-capture-completion"
)

//go:embed complete.bash
var bashScript string

// zshScript is the capture script with carapace-bridge's own configuration
// replaced by the session's: the fpath the live shell reported, and a completion
// dump in the temporary directory rather than in a config directory the machine
// may not have.
var zshScript = patchZsh(zsh.Script)

// zshReplace are the capture script's lines that load carapace-bridge's own zsh
// configuration, and what loads the session's instead. The script is inside a
// single-quoted here-string, so the replacement uses no single quotes.
var zshReplace = [][2]string{
	{`autoload -U compinit && compinit -d "${CARAPACE_BRIDGE_CONFIG_HOME:-$HOME/.config}/carapace/bridge/zsh/.zcompdump_capture"`,
		`[[ -n $VP_FPATH ]] && FPATH=$VP_FPATH; autoload -U compinit && compinit -C -d "${TMPDIR:-/tmp}/vibepod-$UID-zcompdump"`},
	{`source "${CARAPACE_BRIDGE_CONFIG_HOME:-$HOME/.config}/carapace/bridge/zsh/.zshrc"`, ``},
	{`[[ "$oldfpath" != "$fpath" ]] && compinit # second call to adopt any changes to fpath`, ``},
}

func patchZsh(s string) string {
	for _, r := range zshReplace {
		s = strings.Replace(s, r[0], r[1], 1)
	}
	return s
}

// patched reports whether every replacement found its line: an upstream change
// to the script must fail a test rather than load carapace's config quietly.
func patched() error {
	for _, r := range zshReplace {
		if !strings.Contains(zsh.Script, r[0]) {
			return fmt.Errorf("zsh-capture-completion changed: %q is gone", r[0])
		}
	}
	return nil
}

// launcher picks the machine's shell and hands it the line. It is POSIX sh,
// because it runs before anything is known about the machine. In a pod, $SHELL
// is vibepod's recording shell and the user's own is VIBEPOD_REAL_SHELL.
const launcher = `s=${VIBEPOD_REAL_SHELL:-$SHELL}
case "${s##*/}" in
zsh) command -v zsh >/dev/null 2>&1 && exec zsh -f -c "$VP_ZSH" -- "$1" ;;
esac
command -v bash >/dev/null 2>&1 && exec bash --norc --noprofile -c "$VP_BASH" vp-complete "$1"
exit 1`

// Command is what to run, and with what environment added, to complete line.
// fpath is the zsh function path the session's shell reported, or "".
func Command(line, fpath string) (argv, env []string) {
	argv = []string{"sh", "-c", launcher, "vp-complete", line}
	env = []string{"VP_ZSH=" + zshScript, "VP_BASH=" + bashScript}
	if fpath != "" {
		env = append(env, "VP_FPATH="+fpath)
	}
	return argv, env
}

// Candidate is one completion: the whole word it replaces the last one with,
// quoted for the shell as the shell quoted it, and what it is, when the shell
// said.
type Candidate struct {
	Word string
	Desc string
}

// Parse reads what Command printed.
func Parse(out string) []Candidate {
	var cs []Candidate
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		// bash-completion marks "a word, then a space" with the space itself.
		l = strings.TrimRight(l, "\r ")
		if strings.TrimSpace(l) == "" {
			continue
		}
		word, desc, _ := strings.Cut(l, " -- ")
		if seen[word] {
			continue
		}
		seen[word] = true
		cs = append(cs, Candidate{Word: word, Desc: strings.TrimSpace(desc)})
	}
	return cs
}
