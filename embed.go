// Package metakeyguard holds the files compiled into the keyguard binary.
package metakeyguard

import "embed"

// Extension is the Chrome extension that `keyguard install-bridge` copies to Windows.
// The tests directory is left out on purpose.
//
//go:embed extension/*.js extension/*.json extension/*.html extension/*.css extension/*.png
var Extension embed.FS
