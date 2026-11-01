// Package grants checks the macOS permission grants of the processes that are responsible for
// programs (docs/specs/macos-identity.md 4 and 7): the holders, from a probe pane each holder
// runs, and gff-hab itself. gff-hab serves the result as hab.permissions; `gff setup` and the Mac
// app read it from there and check nothing in their own process (7.2).
//
// The probe is `gff-hab probe`, started in a pane, so its responsible process is the holder. It
// prints one line, a marker with a nonce and the result as base64 JSON, and exits. The C checks
// (AXIsProcessTrusted, CGPreflightScreenCaptureAccess, AEDeterminePermissionToAutomateTarget) go
// through purego in darwin-only files, so the package needs no cgo (Q4).
package grants

import (
	"strings"
)

// Grant names: macos-identity.md 7.1, plus the rows 7.3 and 7.6 add.
const (
	Domain          = "domain"            // the graphical (Aqua) launchd domain
	BackgroundItem  = "background-item"   // the jobs are allowed in Login Items
	Signature       = "signature"         // 5.3 rows 1 to 4 on the installed bundle
	FullDiskAccess  = "full-disk-access"  // SystemPolicyAllFiles
	FilesAndFolders = "files-and-folders" // Desktop, Documents, Downloads
	LocalNetwork    = "local-network"
	Accessibility   = "accessibility"
	ScreenRecording = "screen-recording"
	DeveloperTools  = "developer-tools"
	Pasteboard      = "pasteboard"
	DevBuild        = "dev-build"    // a note: an ad-hoc build holds grants until its next rebuild
	RegrantOwed     = "regrant-owed" // 6.4
	Lingering       = "lingering"    // Linux: the user's systemd instance outlives the login
	Units           = "units"        // Linux: the systemd user units
	// AutomationPrefix starts an Automation grant: automation:<target bundle id>.
	AutomationPrefix = "automation:"
)

// Known reports whether name is a grant `gff setup --grant` and hab.permissions take.
func Known(name string) bool {
	switch name {
	case Domain, BackgroundItem, Signature, FullDiskAccess, FilesAndFolders, LocalNetwork,
		Accessibility, ScreenRecording, DeveloperTools, Pasteboard, DevBuild, RegrantOwed,
		Lingering, Units:
		return true
	}
	t, ok := strings.CutPrefix(name, AutomationPrefix)
	return ok && validBundleID(t)
}

// Requestable reports whether a walk can ask macOS for the grant through an API (7.3).
func Requestable(name string) bool {
	switch name {
	case Accessibility, ScreenRecording, LocalNetwork, FilesAndFolders, Pasteboard:
		return true
	}
	return false
}

func validBundleID(s string) bool {
	if s == "" || len(s) > 255 || !strings.Contains(s, ".") {
		return false
	}
	for _, r := range s {
		if !(r == '.' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return !strings.HasPrefix(s, ".") && !strings.HasSuffix(s, ".") && !strings.Contains(s, "..")
}

// Label is the short column name `gff setup --check` prints for a grant (7.6).
func Label(name string) string {
	switch name {
	case Domain:
		return "aqua"
	case BackgroundItem:
		return "login"
	case Signature:
		return "sig"
	case FullDiskAccess:
		return "fda"
	case FilesAndFolders:
		return "files"
	case LocalNetwork:
		return "lan"
	case Accessibility:
		return "ax"
	case ScreenRecording:
		return "screen"
	case DeveloperTools:
		return "devtools"
	case Pasteboard:
		return "paste"
	case DevBuild:
		return "dev"
	case RegrantOwed:
		return "regrant"
	case Lingering:
		return "linger"
	case Units:
		return "units"
	}
	if t, ok := strings.CutPrefix(name, AutomationPrefix); ok {
		return "ae " + t
	}
	return name
}

// Settings URLs (7.3, checked by P8). Local Network has no anchor on macOS 27 (8.1), so its URL
// opens Privacy & Security and the walk names the row in words.
const (
	privacy = "x-apple.systempreferences:com.apple.preference.security"
	// LoginItemsURL opens General > Login Items & Extensions.
	LoginItemsURL = "x-apple.systempreferences:com.apple.LoginItems-Settings.extension"
)

// SettingsURL returns the System Settings URL that changes grant name, or "".
func SettingsURL(name string) string {
	switch name {
	case BackgroundItem:
		return LoginItemsURL
	case FullDiskAccess:
		return privacy + "?Privacy_AllFiles"
	case FilesAndFolders:
		return privacy + "?Privacy_FilesAndFolders"
	case LocalNetwork:
		return privacy
	case Accessibility:
		return privacy + "?Privacy_Accessibility"
	case ScreenRecording:
		return privacy + "?Privacy_ScreenCapture"
	case DeveloperTools:
		return privacy + "?Privacy_DevTools"
	case Pasteboard:
		return privacy + "?Privacy_Pasteboard"
	}
	if strings.HasPrefix(name, AutomationPrefix) {
		return privacy + "?Privacy_Automation"
	}
	return ""
}

// MenuPath is the place in System Settings in words, for when a URL does not open (7.3).
func MenuPath(name string) string {
	const ps = "System Settings > Privacy & Security > "
	switch name {
	case BackgroundItem:
		return "System Settings > General > Login Items & Extensions"
	case FullDiskAccess:
		return ps + "Full Disk Access"
	case FilesAndFolders:
		return ps + "Files & Folders"
	case LocalNetwork:
		return ps + "Local Network"
	case Accessibility:
		return ps + "Accessibility"
	case ScreenRecording:
		return ps + "Screen & System Audio Recording"
	case DeveloperTools:
		return ps + "Developer Tools"
	case Pasteboard:
		return ps + "Paste from Other Apps"
	}
	if strings.HasPrefix(name, AutomationPrefix) {
		return ps + "Automation"
	}
	return ""
}
