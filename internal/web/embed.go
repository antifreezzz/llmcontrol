package web

import (
	"embed"
	"io/fs"
)

//go:embed static/*
var staticEmbed embed.FS

func StaticFS() fs.FS {
	sub, err := fs.Sub(staticEmbed, "static")
	if err != nil {
		return staticEmbed
	}
	return sub
}
