// Package assets embeds launcher resources so the binary is self-contained.
package assets

import "embed"

// BackendLock pins the Xodus backend source, patches and protocol version.
//
//go:embed backend-source.lock.json
var BackendLock []byte

// Patches are applied when building the backend from source.
//
//go:embed *.patch
var Patches embed.FS

//go:embed icon.png
var Icon []byte

// Fonts holds the bundled fallback fonts.
//
//go:embed fonts
var Fonts embed.FS
