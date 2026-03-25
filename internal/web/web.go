package web

import "embed"

//go:embed assets/* index.html
var Assets embed.FS
