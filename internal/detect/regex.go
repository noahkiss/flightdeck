package detect

import "strings"

// goRegex rewrites the parts of the manifests' regex dialect that Go's RE2 syntax spells
// differently. Everything else is passed through, and Go refuses what it cannot compile (for
// example look-around), which makes the manifest unusable rather than wrong.
//
//   - \uXXXX and \u{X...} become \x{...}.
//   - \p{Alphabetic} becomes letters plus letter numbers (\p{L}\p{Nl}), inside a class or as a
//     class of its own. Go has no Alphabetic property; this leaves out a few combining marks.
//
// Known difference left as is: \d, \w, \s and \b are Unicode-aware in the manifests' dialect and
// ASCII in Go. The vendored rules use them around ASCII text.
func goRegex(p string) string {
	var b strings.Builder
	inClass := false
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '\\' && i+1 < len(p):
			next := p[i+1]
			switch {
			case next == 'u' && i+2 < len(p) && p[i+2] == '{':
				if end := strings.IndexByte(p[i+3:], '}'); end >= 0 {
					b.WriteString(`\x{` + p[i+3:i+3+end] + `}`)
					i += 3 + end
					continue
				}
			case next == 'u' && i+6 <= len(p) && isHex(p[i+2:i+6]):
				b.WriteString(`\x{` + p[i+2:i+6] + `}`)
				i += 5
				continue
			case strings.HasPrefix(p[i:], `\p{Alphabetic}`):
				if inClass {
					b.WriteString(`\p{L}\p{Nl}`)
				} else {
					b.WriteString(`[\p{L}\p{Nl}]`)
				}
				i += len(`\p{Alphabetic}`) - 1
				continue
			}
			b.WriteByte(c)
			b.WriteByte(next)
			i++
		case c == '[' && !inClass:
			inClass = true
			b.WriteByte(c)
			// A ] right after [ or [^ is a literal, not the end of the class.
			if i+1 < len(p) && p[i+1] == '^' {
				b.WriteByte('^')
				i++
			}
			if i+1 < len(p) && p[i+1] == ']' {
				b.WriteByte(']')
				i++
			}
		case c == ']' && inClass:
			inClass = false
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
