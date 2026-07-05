package broker

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed web/*
var webFiles embed.FS

func (s *Server) registerWebRoutes() {
	serverFS, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	assetServer := http.FileServer(http.FS(serverFS))
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			s.serveWebFile(w, r, serverFS, "landing.html")
		case "/docs", "/docs/":
			s.serveWebFile(w, r, serverFS, "docs.html")
		case "/share":
			s.serveWebFile(w, r, serverFS, "index.html")
		case "/manifest.webmanifest":
			w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
			s.serveWebFile(w, r, serverFS, "manifest.webmanifest")
		case "/sw.js":
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Header().Set("Service-Worker-Allowed", "/")
			s.serveWebFile(w, r, serverFS, "sw.js")
		default:
			if strings.HasPrefix(r.URL.Path, "/assets/") {
				assetServer.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}
	})
}

func (s *Server) serveWebFile(w http.ResponseWriter, r *http.Request, serverFS fs.FS, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if strings.Contains(path.Clean(name), "..") {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, serverFS, name)
}
