package gffclient

import (
	"strings"
	"unicode"
)

const DefaultTitleTemplate = "{host} · {workspace} | {title}"

func cleanTitle(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			continue
		}
		if n == 512 {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func (c *client) updateTitle() {
	template := DefaultTitleTemplate
	if c.cfg.TitleTemplate != nil {
		template = *c.cfg.TitleTemplate
	}
	if template == "" {
		return
	}
	host, title := "", ""
	if v := c.focused(); v != nil {
		host, title = v.h.label, v.filter.title
		if title == "" {
			title = v.spec.Label
		}
	}
	title = cleanTitle(strings.NewReplacer("{host}", host, "{workspace}", c.cfg.Workspace, "{title}", title).Replace(template))
	if c.titleSaved && title == c.windowTitle {
		return
	}
	if !c.titleSaved {
		c.out.WriteString("\x1b[22;2t")
		c.titleSaved = true
	}
	c.out.WriteString("\x1b]2;" + title + "\x1b\\")
	c.windowTitle = title
}
