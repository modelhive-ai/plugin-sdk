package plugin

import (
	"net/url"
	"path/filepath"
	"strings"
)

// UnixSocketURL is the unix:// URL of a socket file. A Windows path becomes
// unix:///C:/dir/plugin.sock, so the URL path is absolute on every platform.
func UnixSocketURL(socket string) string {
	p := filepath.ToSlash(socket)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "unix", Path: p}).String()
}

// UnixSocketPath is the socket file of a unix:// URL path; the inverse of
// UnixSocketURL.
func UnixSocketPath(urlPath string) string {
	if len(urlPath) >= 3 && urlPath[0] == '/' && urlPath[2] == ':' {
		urlPath = urlPath[1:] // drive letter: /C:/dir -> C:/dir
	}
	return filepath.FromSlash(urlPath)
}
