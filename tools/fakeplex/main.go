// Command fakeplex is a tiny stand-in for the Plex HTTP API used to exercise
// plex-4k-guard end to end without a real Plex Media Server. It serves
// fixture XML for /status/sessions and /library/metadata/{ratingKey},
// answers /identity, and logs every /status/sessions/terminate call with
// the decoded sessionId and reason. It never logs the X-Plex-Token value.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:32499", "address to listen on")
	sessions := flag.String("sessions", "", "path to the XML served for /status/sessions")
	metadataDir := flag.String("metadata-dir", "", "directory with metadata_<ratingKey>.xml files served for /library/metadata/<ratingKey>")
	token := flag.String("token", "", "when set, authenticated endpoints require this X-Plex-Token header value")
	flag.Parse()
	if *sessions == "" || *metadataDir == "" {
		fmt.Fprintln(os.Stderr, "usage: fakeplex -sessions sessions.xml -metadata-dir dir [-listen addr] [-token expected]")
		os.Exit(2)
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("[fakeplex] ")

	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if *token != "" && r.Header.Get("X-Plex-Token") != *token {
				log.Printf("%s %s -> 401 (token header %s)", r.Method, r.URL.Path, presence(r.Header.Get("X-Plex-Token")))
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/identity", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s -> 200", r.Method, r.URL.Path)
		w.Header().Set("Content-Type", "text/xml;charset=utf-8")
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><MediaContainer size="0" claimed="1" machineIdentifier="fakeplex" version="1.43.4.10903"></MediaContainer>`)
	})
	mux.HandleFunc("/status/sessions", authed(func(w http.ResponseWriter, r *http.Request) {
		serveFile(w, r, *sessions)
	}))
	mux.HandleFunc("/library/metadata/", authed(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/library/metadata/")
		if key == "" || strings.Contains(key, "/") || strings.Contains(key, "..") {
			http.NotFound(w, r)
			return
		}
		serveFile(w, r, filepath.Join(*metadataDir, "metadata_"+key+".xml"))
	}))
	mux.HandleFunc("/status/sessions/terminate", authed(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		log.Printf("TERMINATE method=%s sessionId=%q reason=%q", r.Method, q.Get("sessionId"), q.Get("reason"))
		w.WriteHeader(http.StatusOK)
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s -> 404", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})
	log.Printf("listening on %s (sessions=%s metadata=%s auth=%s)", *listen, *sessions, *metadataDir, presence(*token))
	srv := &http.Server{Addr: *listen, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func serveFile(w http.ResponseWriter, r *http.Request, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("%s %s -> 404 (%v)", r.Method, r.URL.Path, err)
		http.NotFound(w, r)
		return
	}
	log.Printf("%s %s -> 200", r.Method, r.URL.Path)
	w.Header().Set("Content-Type", "text/xml;charset=utf-8")
	_, _ = w.Write(data)
}

func presence(s string) string {
	if s == "" {
		return "absent"
	}
	return "present"
}
