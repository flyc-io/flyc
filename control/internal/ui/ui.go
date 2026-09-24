// Package ui : interface d'administration embarquée (HTML/CSS/JS sans étape de build), servie par flyc-control.
package ui

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed static/*
var static embed.FS

var types = map[string]string{"index.html": "text/html; charset=utf-8", "app.js": "text/javascript; charset=utf-8", "app.css": "text/css; charset=utf-8"}

// Handler sert l'application ; la racine rend index.html (routage par ancre côté client).
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		ct, ok := types[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		data, err := static.ReadFile("static/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", map[bool]string{true: "no-store", false: "no-cache"}[name == "index.html"])
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		_, _ = w.Write(data)
	})
}
