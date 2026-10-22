// Package clients holds the reference clients of the /api/v1 read API: the
// userscript and the DevTools extension, which Inspector serves to install.
package clients

import "embed"

//go:embed userscript extension
var Files embed.FS
