package grants

import (
	"context"
	"fmt"
	"strings"

	"github.com/noahkiss/flightdeck/api"
)

// Runner runs a command and returns its combined output. A non-zero exit is an error.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// SigInfo is what the signature check learned about one installed bundle.
type SigInfo struct {
	Check
	// Requirement is the designated requirement, as codesign prints it; "" when unread.
	Requirement string
	// AdHoc means the requirement is a code hash: grants last until the next rebuild (3.5).
	AdHoc bool
}

// ReleaseRequirement is the 5.1 template for a Developer ID signature of identifier id by team.
func ReleaseRequirement(id, team string) string {
	return `identifier "` + id + `" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] /* exists */ and certificate leaf[field.1.2.840.113635.100.6.1.13] /* exists */ and certificate leaf[subject.OU] = "` + team + `"`
}

// normReq makes two requirement texts comparable: codesign prints the team unquoted where the
// template quotes it (5.2.1), and spacing may differ.
func normReq(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, `"`, "")), " ")
}

// SameRequirement reports whether two designated requirements are the same text.
func SameRequirement(a, b string) bool { return a != "" && normReq(a) == normReq(b) }

// codesignFields reads Key=value lines of `codesign -dv --verbose=4`. Authority repeats; the
// first is the leaf.
func codesignFields(out []byte) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if _, seen := m[k]; !seen {
			m[k] = v
		}
	}
	return m
}

// designated returns the designated requirement of item.
func designated(ctx context.Context, run Runner, item string) (string, error) {
	out, err := run(ctx, "codesign", "-d", "-r-", item)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if _, r, ok := strings.Cut(line, "designated => "); ok {
			return strings.TrimSpace(r), nil
		}
	}
	return "", fmt.Errorf("codesign printed no designated requirement for %s", item)
}

// CheckSignature runs 5.3 rows 1 to 4 on an installed bundle, and reads whether it is notarized
// (row 5, reported, not gated). A Developer ID bundle must carry the release requirement; an
// ad-hoc one is a dev build; an Apple Development one is a dev build signed with
// GFF_DEV_SIGN_IDENTITY.
func CheckSignature(ctx context.Context, run Runner, bundle string) SigInfo {
	si := SigInfo{Check: Check{Grant: Signature}}
	fail := func(format string, a ...any) SigInfo {
		si.State, si.Detail = api.PermissionStateDenied, fmt.Sprintf(format, a...)
		return si
	}
	if _, err := run(ctx, "codesign", "--verify", "--strict", "--verbose=2", bundle); err != nil {
		return fail("signature or seal is broken: reinstall the release (%s)", firstLine(err.Error()))
	}
	out, err := run(ctx, "codesign", "-dv", "--verbose=4", bundle)
	if err != nil {
		return fail("codesign -dv: %s", firstLine(err.Error()))
	}
	f := codesignFields(out)
	id, team, auth := f["Identifier"], f["TeamIdentifier"], f["Authority"]
	if si.Requirement, err = designated(ctx, run, bundle); err != nil {
		return fail("%s", firstLine(err.Error()))
	}
	if strings.HasPrefix(si.Requirement, "cdhash") {
		si.AdHoc = true
		si.State, si.Detail = api.PermissionStateGranted, id+", ad-hoc signature (dev build)"
		return si
	}
	if !strings.Contains(f["flags"], "runtime") {
		return fail("%s is signed without the hardened runtime", id)
	}
	if team == "" || team == "not set" {
		return fail("%s names no team", id)
	}
	if !strings.HasPrefix(auth, "Developer ID Application") {
		si.State, si.Detail = api.PermissionStateGranted, fmt.Sprintf("%s, team %s, %s (dev build)", id, team, kind(auth))
		return si
	}
	tmpl := ReleaseRequirement(id, team)
	if !SameRequirement(si.Requirement, tmpl) {
		return fail("%s carries a requirement that is not the release one: grants will not carry over", id)
	}
	if _, err := run(ctx, "codesign", "--verify", "-R="+tmpl, bundle); err != nil {
		return fail("%s does not satisfy the release requirement", id)
	}
	detail := fmt.Sprintf("%s, team %s, Developer ID", id, team)
	if out, err := run(ctx, "spctl", "--assess", "--type", "execute", "-vv", bundle); err == nil && strings.Contains(string(out), "Notarized Developer ID") {
		detail += ", notarized"
	} else {
		detail += ", not notarized"
	}
	si.State, si.Detail = api.PermissionStateGranted, detail
	return si
}

func kind(auth string) string {
	if k, _, ok := strings.Cut(auth, ":"); ok {
		return k
	}
	if auth == "" {
		return "unknown certificate"
	}
	return auth
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
