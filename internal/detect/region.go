package detect

import (
	"strconv"
	"strings"
)

// Regions name the part of the screen a rule reads. The names and their meaning follow the
// manifest format:
//
//   - whole_recent: the whole screen text.
//   - bottom_lines(N): the last N lines.
//   - bottom_non_empty_lines(N): from the Nth non-empty line counted from the bottom to the end.
//   - top_non_empty_lines(N): from the start through the Nth non-empty line.
//   - osc_title, osc_progress: the last OSC title or progress report, not the screen.
//   - after_last_horizontal_rule: the lines after the last rule line (a run of ─).
//   - prompt_box_body, above_prompt_box, last_non_empty_above_prompt_box: around the prompt
//     box, whose top border is the second rule line counted from the bottom.
//   - after_last_prompt_marker, before_current_prompt_marker,
//     whole_recent_without_current_prompt_marker, current_prompt_block_marker,
//     after_current_prompt_block_marker: around the prompt line (› ...). The prompt is current
//     when no block marker line (•, ■, ✗, ✓) follows it.
//
// A region that cannot be found reads as the whole screen where the format says so, and as the
// empty string otherwise.
var fixedRegions = map[string]bool{
	"whole_recent":                               true,
	"after_last_prompt_marker":                   true,
	"before_current_prompt_marker":               true,
	"whole_recent_without_current_prompt_marker": true,
	"current_prompt_block_marker":                true,
	"after_current_prompt_block_marker":          true,
	"prompt_box_body":                            true,
	"above_prompt_box":                           true,
	"last_non_empty_above_prompt_box":            true,
	"after_last_horizontal_rule":                 true,
	"osc_title":                                  true,
	"osc_progress":                               true,
}

func validRegion(spec string) bool {
	if fixedRegions[spec] {
		return true
	}
	for _, name := range []string{"bottom_lines", "bottom_non_empty_lines", "top_non_empty_lines"} {
		if _, ok := regionCount(spec, name); ok {
			return true
		}
	}
	return false
}

// regionCount parses name(N) with N a positive whole number of at most 65535.
func regionCount(spec, name string) (int, bool) {
	rest, ok := strings.CutPrefix(spec, name+"(")
	if !ok {
		return 0, false
	}
	num, ok := strings.CutSuffix(rest, ")")
	if !ok || num == "" || num[0] == '0' {
		return 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// line is one line of the screen text: [start, end) excludes the newline and a trailing CR;
// next is the offset of the following line.
type line struct{ start, end, next int }

func lineSpans(s string) []line {
	var out []line
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '\n')
		if j < 0 {
			out = append(out, line{i, len(s), len(s)})
			break
		}
		end := i + j
		if end > i && s[end-1] == '\r' {
			end--
		}
		out = append(out, line{i, end, i + j + 1})
		i += j + 1
	}
	return out
}

func splitLines(s string) []string {
	spans := lineSpans(s)
	out := make([]string, len(spans))
	for i, l := range spans {
		out[i] = s[l.start:l.end]
	}
	return out
}

func region(in Input, spec string) string {
	switch spec {
	case "osc_title":
		return in.OSCTitle
	case "osc_progress":
		return in.OSCProgress
	}
	s := in.Screen
	ls := lineSpans(s)
	text := func(i int) string { return s[ls[i].start:ls[i].end] }
	from := func(i int) string { // from the start of line i to the end
		if i >= len(ls) {
			return ""
		}
		return s[ls[i].start:]
	}
	upTo := func(i int) string { // from the start to the start of line i
		if i >= len(ls) {
			return s
		}
		return s[:ls[i].start]
	}
	empty := func(i int) bool { return strings.TrimSpace(text(i)) == "" }

	isPrompt := func(i int) bool { t := text(i); return t == "›" || strings.HasPrefix(t, "› ") }
	isBlock := func(i int) bool {
		t := text(i)
		return strings.HasPrefix(t, "•") || strings.HasPrefix(t, "■") || strings.HasPrefix(t, "✗") || strings.HasPrefix(t, "✓")
	}
	lastPrompt := func() int {
		for i := len(ls) - 1; i >= 0; i-- {
			if isPrompt(i) {
				return i
			}
		}
		return -1
	}
	currentPrompt := func() int {
		p := lastPrompt()
		if p < 0 {
			return -1
		}
		for i := p + 1; i < len(ls); i++ {
			if isBlock(i) {
				return -1
			}
		}
		return p
	}
	blockBeforeCurrent := func() int {
		p := currentPrompt()
		for i := p - 1; p >= 0 && i >= 0; i-- {
			if isBlock(i) {
				return i
			}
		}
		return -1
	}
	boxTop := func() int { // the second rule line from the bottom
		n := 0
		for i := len(ls) - 1; i >= 0; i-- {
			if isHorizontalRule(text(i)) {
				if n++; n == 2 {
					return i
				}
			}
		}
		return -1
	}
	aboveBox := func() string {
		if t := boxTop(); t >= 0 {
			return upTo(t)
		}
		return s
	}

	switch spec {
	case "whole_recent":
		return s
	case "after_last_prompt_marker":
		if p := lastPrompt(); p >= 0 {
			return from(p + 1)
		}
		return s
	case "before_current_prompt_marker":
		if p := currentPrompt(); p >= 0 {
			return upTo(p)
		}
		return s
	case "whole_recent_without_current_prompt_marker":
		if currentPrompt() >= 0 {
			return ""
		}
		return s
	case "current_prompt_block_marker":
		if b := blockBeforeCurrent(); b >= 0 {
			return text(b)
		}
		return ""
	case "after_current_prompt_block_marker":
		if b := blockBeforeCurrent(); b >= 0 {
			return from(b)
		}
		return ""
	case "prompt_box_body":
		t := boxTop()
		if t < 0 {
			return ""
		}
		end := len(ls)
		for i := t + 1; i < len(ls); i++ {
			if isHorizontalRule(text(i)) {
				end = i
				break
			}
		}
		if t+1 >= len(ls) {
			return ""
		}
		return s[ls[t+1].start:upToOffset(s, ls, end)]
	case "above_prompt_box":
		return aboveBox()
	case "last_non_empty_above_prompt_box":
		above := splitLines(aboveBox())
		for i := len(above) - 1; i >= 0; i-- {
			if strings.TrimSpace(above[i]) != "" {
				return above[i]
			}
		}
		return ""
	case "after_last_horizontal_rule":
		for i := len(ls) - 1; i >= 0; i-- {
			if isHorizontalRule(text(i)) {
				return s[ls[i].next:]
			}
		}
		return s
	}
	if n, ok := regionCount(spec, "bottom_lines"); ok {
		return from(max(0, len(ls)-n))
	}
	if n, ok := regionCount(spec, "bottom_non_empty_lines"); ok {
		start, seen := -1, 0
		for i := len(ls) - 1; i >= 0 && seen < n; i-- {
			if !empty(i) {
				start, seen = i, seen+1
			}
		}
		if start < 0 {
			return ""
		}
		return from(start)
	}
	if n, ok := regionCount(spec, "top_non_empty_lines"); ok {
		last, seen := -1, 0
		for i := 0; i < len(ls) && seen < n; i++ {
			if !empty(i) {
				last, seen = i, seen+1
			}
		}
		if last < 0 {
			return ""
		}
		return s[:ls[last].next]
	}
	return ""
}

func upToOffset(s string, ls []line, i int) int {
	if i >= len(ls) {
		return len(s)
	}
	return ls[i].start
}

// isHorizontalRule: a trimmed line that starts with ─ and is either all ─, or has at least three
// ─ before other text (a rule with a label).
func isHorizontalRule(l string) bool {
	t := strings.TrimSpace(l)
	if t == "" {
		return false
	}
	n := 0
	rest := t
	for strings.HasPrefix(rest, "─") {
		n++
		rest = rest[len("─"):]
	}
	if n == 0 {
		return false
	}
	return strings.TrimSpace(rest) == "" || n >= 3
}
