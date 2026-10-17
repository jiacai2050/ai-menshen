package web

import "embed"

//go:embed assets/* index.html logs.html
var Assets embed.FS
