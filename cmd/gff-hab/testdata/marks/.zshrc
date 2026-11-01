# Test fixture: makes an interactive zsh emit OSC 133 prompt marks (hab-logic-api.md 2.5).
# Not the console's shipped snippet. Run: ZDOTDIR=<this folder> zsh -d -i
unset HISTFILE
unsetopt BEEP
# zsh prints its partial-line mark (PROMPT_SP, PROMPT_CR) before precmd, so with it on that mark
# would land inside the output range, before D. A shipped snippet must handle that too.
unsetopt PROMPT_SP PROMPT_CR
# D with the last status, before every prompt (and once before the first: the hab ignores it).
_gff_precmd() { print -rn -- $'\e]133;D;'$?$'\a' }
# C with the command line, percent-encoded, after the line is read.
_gff_preexec() {
  local s=$1 out= c
  local -i i
  for (( i = 1; i <= ${#s}; i++ )); do
    c=$s[i]
    if [[ $c == [A-Za-z0-9._~-] ]]; then out+=$c; else out+=$(printf '%%%02X' "'$c"); fi
  done
  print -rn -- $'\e]133;C;cmdline_url='$out$'\a'
}
typeset -ga precmd_functions preexec_functions
precmd_functions=(_gff_precmd)
preexec_functions=(_gff_preexec)
# A, the prompt, B; the typed line follows.
PS1=$'%{\e]133;A\a%}$ %{\e]133;B\a%}'
RPS1=
