//go:build ghostty

package gffclient

// With the ghostty tag, the gff binary under test uses libghostty, as release builds do.
func init() { gffTags = append(gffTags, "ghostty") }
