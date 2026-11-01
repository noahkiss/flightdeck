package detect

import (
	"regexp"
	"strings"
)

// Tested is the newest harness version each adapter was tested against, as a version prefix:
// "2.1" covers every 2.1.x. A version is newer when its leading segments, as many as the prefix
// has, compare above the prefix. Raise an entry only after the adapter's fixtures pass on the
// newer version. A harness with no entry is never flagged.
var Tested = map[string]string{
	KindClaude:   "2.1",
	KindCodex:    "0.166",
	KindOpencode: "2.2",
}

// Reasons a VersionWarning gives.
const (
	VersionNewer      = "newer_than_tested"
	VersionUnreadable = "unreadable"
)

// VersionWarning says a harness reported a version the adapter was not tested on. gff-hab shows
// it on the session in the status board; it does not stop the adapter.
type VersionWarning struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Tested  string `json:"tested"`
	Reason  string `json:"reason"`
}

var leadingVersion = regexp.MustCompile(`\d+(?:\.\d+)*`)

// CheckVersion compares a harness's reported version with Tested. It returns nil when the
// version is within the tested range, when the harness has no entry, or when version is empty
// (nothing reported yet). A version with no number in it is unreadable.
func CheckVersion(kind, version string) *VersionWarning {
	max, ok := Tested[kind]
	if !ok || strings.TrimSpace(version) == "" {
		return nil
	}
	v := leadingVersion.FindString(version)
	if v == "" {
		return &VersionWarning{Kind: kind, Version: version, Tested: max, Reason: VersionUnreadable}
	}
	n := len(strings.Split(max, "."))
	segs := strings.Split(v, ".")
	if len(segs) > n {
		segs = segs[:n]
	}
	if CompareVersions(strings.Join(segs, "."), max) > 0 {
		return &VersionWarning{Kind: kind, Version: version, Tested: max, Reason: VersionNewer}
	}
	return nil
}
