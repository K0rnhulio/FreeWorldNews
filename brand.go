package main

import (
	_ "embed"
	"net/http"
)

//go:embed docs/assets/favicon.svg
var brandIcon []byte

func brandIconHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(brandIcon)
}
