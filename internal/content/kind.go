// Package content serves rich content for the chat view (docs/specs/app.md 4.7): files an agent
// showed with content.show, images its tool calls touched, and files the session changed, with
// their bytes in bounded chunks and their diffs. The hab stores a reference to each file on its
// host, never a copy, and reads only files a session referenced.
package content

import (
	"crypto/sha256"
	"encoding/hex"
	"mime"
	"path/filepath"
	"strings"

	"github.com/noahkiss/flightdeck/api"
)

// Limits (app.md 4.7, built choices).
const (
	MaxItems     = 500       // items in one content.list
	MaxShown     = 200       // stored references per session
	MaxFile      = 32 << 20  // the largest file content.read serves
	DefaultChunk = 256 << 10 // content.read's default run
	MaxChunk     = 1 << 20   // content.read's largest run
	MaxDiff      = 512 << 10 // the largest diff content.diff returns
	MaxTitle     = 200       // characters of a shown title
)

var imageExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".heic": true,
	".heif": true, ".tif": true, ".tiff": true, ".bmp": true,
}

var textExt = map[string]bool{
	".txt": true, ".log": true, ".json": true, ".jsonl": true, ".yaml": true, ".yml": true,
	".toml": true, ".csv": true, ".xml": true, ".go": true, ".swift": true, ".py": true,
	".js": true, ".ts": true, ".tsx": true, ".jsx": true, ".rs": true, ".c": true, ".h": true,
	".sh": true, ".css": true, ".sql": true, ".kdl": true, ".svg": true, ".diff": true,
	".patch": true, ".ini": true, ".conf": true,
}

// IsImage says the file name names an image the apps can render.
func IsImage(name string) bool { return imageExt[strings.ToLower(filepath.Ext(name))] }

// KindOf picks how a client renders a file, from its name.
func KindOf(name string) api.ContentKind {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case imageExt[ext]:
		return api.ContentKindImage
	case ext == ".md" || ext == ".markdown":
		return api.ContentKindMarkdown
	case ext == ".pdf":
		return api.ContentKindPdf
	case ext == ".html" || ext == ".htm":
		return api.ContentKindHtml
	case ext == ".diff" || ext == ".patch":
		return api.ContentKindDiff
	case textExt[ext]:
		return api.ContentKindText
	}
	return api.ContentKindFile
}

// TypeOf is a file's media type, from its name.
func TypeOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".md", ".markdown":
		return "text/markdown"
	case ".heic":
		return "image/heic"
	case ".heif":
		return "image/heif"
	case ".webp":
		return "image/webp"
	case ".jsonl":
		return "application/jsonl"
	case ".diff", ".patch":
		return "text/x-diff"
	case "":
		return "application/octet-stream"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		if i := strings.IndexByte(t, ';'); i >= 0 {
			t = t[:i]
		}
		return t
	}
	if textExt[ext] {
		return "text/plain"
	}
	return "application/octet-stream"
}

// ID is an item's id: the same for a session and path, so a shown file the session also changed
// is one item.
func ID(session, path string) string {
	h := sha256.Sum256([]byte(session + "\x00" + path))
	return "c" + hex.EncodeToString(h[:8])
}
