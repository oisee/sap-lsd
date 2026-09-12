package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"

	"github.com/oisee/sap-lsd/internal/demo"
	"github.com/oisee/sap-lsd/internal/frame"
	"github.com/oisee/sap-lsd/internal/tui"
)

// serveComposer runs the live timeline composer: a browser edits the show, it
// saves to the compose-file at path, and the demo renderer re-reads that file
// for every new connection (see currentShow), so an edit reaches the next SAP
// GUI to connect. Each scene carries a few rendered frames as a thumbnail, made
// by running the scene through the same tui grid renderer the terminal uses.
func serveComposer(addr, path string, log func(string, ...any)) {
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(composerHTML))
	})

	// GET returns the current compose-file (or an empty show); POST writes it.
	mux.HandleFunc("/api/show", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			b, err := os.ReadFile(path)
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				_, _ = w.Write([]byte(`{"sceneMs":5000,"show":[]}`))
				return
			}
			_, _ = w.Write(b)
		case http.MethodPost:
			var sf showFile // validate it parses as a compose-file
			if err := json.NewDecoder(r.Body).Decode(&sf); err != nil {
				http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			b, _ := json.MarshalIndent(sf, "", "  ")
			if err := os.WriteFile(path, b, 0o644); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			log("show saved (%d scenes) -> %s", len(sf.Show), path)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/thumbs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(thumbsJSON())
	})

	log("live composer on http://%s  (edits save to %s; new connections play them)", addr, path)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log("http server stopped: %v", err)
	}
}

var (
	thumbsOnce sync.Once
	thumbsCard []byte
)

// thumbsJSON renders every scene to a handful of ASCII frames once and caches
// the result: [{name, frames:[...]}]. Dynpro scenes go through tui.Render; the
// LED (list-channel) scenes through tui.RenderList — the same rasterisers the
// terminal client draws with, so the thumbnail is what the scene really looks
// like.
func thumbsJSON() []byte {
	thumbsOnce.Do(func() {
		type card struct {
			Name   string   `json:"name"`
			Frames []string `json:"frames"`
		}
		tss := []float64{1.5, 3.5, 5.5, 7.5}
		var out []card
		for _, s := range demo.Scenes() {
			var frames []string
			for _, ts := range tss {
				var g *tui.Grid
				if eff := demo.LEDEffectIndex(s.Name); eff >= 0 {
					g = tui.RenderList(demo.LEDSegmentsEff(eff, ts), 22, 120)
				} else if s.Dynpro != nil {
					scr := frame.New(27, 120)
					s.Dynpro(ts, scr)
					g = tui.Render(scr.Atoms(), 22, 120)
				} else {
					continue
				}
				frames = append(frames, g.String())
			}
			out = append(out, card{Name: s.Name, Frames: frames})
		}
		thumbsCard, _ = json.Marshal(out)
	})
	return thumbsCard
}
