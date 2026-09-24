# Tab completion for one line, from bash's own completion system: what
# bash-completion would offer at the end of $1 in an interactive bash, printed one
# candidate per line, each as the whole word it would replace. Run with --norc,
# so nothing of the user's is changed and nothing is printed but candidates.
line=$1
read -ra COMP_WORDS <<< "$line"
case "$line" in '' | *' ') COMP_WORDS+=("") ;; esac
COMP_CWORD=$((${#COMP_WORDS[@]} - 1))
COMP_LINE=$line
COMP_POINT=${#line}
cur=${COMP_WORDS[COMP_CWORD]}

for f in /usr/share/bash-completion/bash_completion \
	/usr/local/share/bash-completion/bash_completion /etc/bash_completion; do
	[ -r "$f" ] && { . "$f" >/dev/null 2>&1; break; }
done

candidates() {
	if ((COMP_CWORD == 0)); then
		case "$cur" in
		*/*) compgen -f -- "$cur" ;;
		*) compgen -A function -abck -- "$cur" | sort -u ;;
		esac
		return
	fi
	local cmd=${COMP_WORDS[0]##*/}
	if ! complete -p "$cmd" >/dev/null 2>&1; then
		if declare -F __load_completion >/dev/null; then
			__load_completion "$cmd" >/dev/null 2>&1
		elif declare -F _completion_loader >/dev/null; then
			_completion_loader "$cmd" >/dev/null 2>&1
		fi
	fi
	local spec fn
	spec=$(complete -p "$cmd" 2>/dev/null)
	if [[ $spec =~ -F\ ([^ ]+) ]]; then
		fn=${BASH_REMATCH[1]}
		COMPREPLY=()
		"$fn" "$cmd" "$cur" "${COMP_WORDS[COMP_CWORD - 1]}" >/dev/null 2>&1
		printf '%s\n' "${COMPREPLY[@]}"
	else
		compgen -f -- "$cur"
	fi
}

candidates | while IFS= read -r c; do
	[ -z "$c" ] && continue
	if [ -d "$c" ] && [ "${c%/}" = "$c" ]; then c="$c/"; fi
	printf '%s\n' "$c"
done
