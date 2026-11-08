// Package fileuri converts LSP and DAP file URIs to local filesystem paths.
package fileuri

import (
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// FromURI converts a file URI to a local path. It returns false for other or
// malformed URIs so callers can preserve their existing non-file handling.
func FromURI(uri string) (string, bool) {
	return pathFromURI(uri, runtime.GOOS == "windows")
}

func pathFromURI(uri string, windows bool) (string, bool) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Scheme != "file" {
		return "", false
	}

	path := parsed.Path
	if windows {
		if parsed.Host != "" && !strings.EqualFold(parsed.Host, "localhost") {
			path = "//" + parsed.Host + path
		} else if hasWindowsDrivePrefix(path) {
			// file:///D:/workspaces/project has URL path /D:/workspaces/project.
			// Drop the URL's leading slash before filepath.Abs sees it as a
			// drive-less rooted path and prepends the current drive (D:\D:\...).
			path = path[1:]
		}
	}
	return filepath.FromSlash(path), true
}

func hasWindowsDrivePrefix(path string) bool {
	return len(path) >= 3 && path[0] == '/' &&
		((path[1] >= 'A' && path[1] <= 'Z') || (path[1] >= 'a' && path[1] <= 'z')) &&
		path[2] == ':' && (len(path) == 3 || path[3] == '/')
}
