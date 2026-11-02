// Package speedtest embeds the approved command-center interface.
package speedtest

import "embed"

// UI contains the static interface preview. Consumers can use fs.Sub(UI, "ui")
// to serve its files from the root of the filesystem.
//
//go:embed all:ui
var UI embed.FS
