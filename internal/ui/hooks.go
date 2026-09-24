package ui

import "strings"

// The hooks `vp shell` types into a shell, once, when it first sees it.
//
// Typed, not installed: the shell may be on a machine where nothing of
// vibepod's exists and nothing may be written, and typing is the one thing
// that works the same in the pod, over ssh, and inside a node pod. The shell
// stays the user's own — their rc files, their prompt, their history — and
// only learns to say where its prompt and each command's output begin and end.
//
// Each hook is idempotent, so a second visit, or a reattach that cannot tell
// whether the hooks are in, costs nothing. The prompt is wrapped rather than
// replaced, after every other precmd has had its turn, so a raw `vp attach`
// still shows the user's own prompt and a prompt that rebuilds itself each
// time (starship, p10k) is wrapped again each time. The line starts with a
// space and asks for history to ignore it, so it does not become the first
// thing ↑ finds.
var hookLines = []string{
	`if [ -n "$ZSH_VERSION" ]; then`,
	`setopt HIST_IGNORE_SPACE;`,
	`__vp_s(){ local r=$?; printf '\033]133;D;%s\007\033]7717;home=%s;cwd=%s\007' "$r" "$HOME" "$PWD"; };`,
	`__vp_e(){ printf '\033]133;C\007'; };`,
	`__vp_f(){ [[ $FPATH == "$__vp_fp" ]] || { __vp_fp=$FPATH; printf '\033]7717;fpath=%s\007' "$FPATH"; }; };`,
	`__vp_p(){ [[ $PS1 == *'133;B'* ]] || PS1=$'%{\e]133;A\a%}'"$PS1"$'%{\e]133;B\a%}'; };`,
	`precmd_functions=(__vp_s ${precmd_functions:#__vp_[sfp]} __vp_f __vp_p);`,
	`preexec_functions=(${preexec_functions:#__vp_e} __vp_e);`,
	`elif [ -n "$BASH_VERSION" ]; then`,
	`HISTCONTROL="ignorespace:$HISTCONTROL"; history -d $((HISTCMD-1)) 2>/dev/null;`,
	`__vp_s(){ local r=$?; printf '\033]133;D;%s\007\033]7717;home=%s;cwd=%s\007' "$r" "$HOME" "$PWD"; };`,
	`__vp_p(){ case "$PS1" in *'133;B'*) ;; *) PS1='\[\033]133;A\007\]'"$PS1"'\[\033]133;B\007\]';; esac; };`,
	`case "$PROMPT_COMMAND" in *__vp_s*) ;; *) PROMPT_COMMAND=$'__vp_s\n'"$PROMPT_COMMAND"$'\n__vp_p';; esac;`,
	`case "$PS0" in *'133;C'*) ;; *) PS0="$PS0"$'\e]133;C\a';; esac;`,
	`fi`,
}

// Hooks is the line to type.
func Hooks() string { return " " + strings.Join(hookLines, " ") + "\r" }
