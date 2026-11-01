# Test fixture: makes an interactive bash emit OSC 133 prompt marks (hab-logic-api.md 2.5).
# Not the console's shipped snippet. Run: bash --noprofile --rcfile marks.bash -i
# Needs bash 4.4 or newer (PS0).
HISTFILE=
bind 'set enable-bracketed-paste off' 2>/dev/null
# D with the last status, before every prompt (and once before the first: the hab ignores it).
PROMPT_COMMAND='printf "\033]133;D;%s\007" "$?"'
# A, the prompt, B; the typed line follows.
PS1='\[\033]133;A\007\]$ \[\033]133;B\007\]'
# C after the line is read, before the command runs.
PS0='\033]133;C\007'
