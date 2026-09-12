// server is the rogue DIAG server, in its first form: a replay. A real SAP
// GUI connects to it, and it answers with the frames a real server sent in
// a capture — the logon screen, the start menu — and then pushes screens
// of its own making at its own cadence, the way Phase 0 showed a server
// may. What the GUI checks and what it lets pass is learned here.
//
//	server -capture captures/probe.jsonl -mode logon     # answer as the capture did, from the first frame
//	server -capture captures/probe.jsonl -mode menu      # skip the logon: the first client frame gets the start menu
//	server -capture captures/probe.jsonl -mode counter   # menu, then the probe's screen and a counter pushed every 300 ms
package main

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oisee/sap-lsd/internal/ni"

	"github.com/oisee/sap-lsd/internal/alv"
	"github.com/oisee/sap-lsd/internal/demo"
	"github.com/oisee/sap-lsd/internal/diag"
	"github.com/oisee/sap-lsd/internal/frame"
	"github.com/oisee/sap-lsd/internal/replay"
)

// Thin aliases to the shared demo engine (pkg/demo), kept so the server's own
// push renderers and the LED list renderer read as before.
func triangle(x float64, span int) int     { return demo.Triangle(x, span) }
func ledSegments(n int) []diag.ListSegment { return demo.LEDSegments(n) }
func centre(text string, width int) string { return demo.Centre(text, width) }

var capturePath string
var animMsgType byte
var animMsgLoop int
var demoSceneMS int

// showStep is one step of a composed show: a scene by name, how long it runs
// (0 = the default) and a time-speed multiplier for its animation (0/1 = as-is).
type showStep struct {
	name  string
	dur   time.Duration
	speed float64
}

// demoShow is the composed show (from -playlist or -show); empty plays all
// scenes.
var demoShow []showStep

// showPath is the live compose-file (from -show, or defaulted under -http). The
// demo renderer re-reads it for each new connection, and the -http composer
// writes it, so an edit reaches the next SAP GUI that connects — no restart.
var showPath string

// currentShow returns the live show: the compose-file at showPath if it loads,
// otherwise the show fixed at startup. Read fresh per connection.
func currentShow() []showStep {
	if showPath != "" {
		if steps, _, err := loadShow(showPath); err == nil {
			return steps // an empty file (no steps) means "play all", like no show
		}
	}
	return demoShow
}

// parsePlaylist reads "orbit:6,tornado:10,solid" into show steps: each is a
// scene name and an optional seconds after a colon (speed defaults to 1).
func parsePlaylist(spec string) []showStep {
	var out []showStep
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		e := showStep{name: part, speed: 1}
		if i := strings.IndexByte(part, ':'); i >= 0 {
			e.name = strings.TrimSpace(part[:i])
			var sec float64
			if _, err := fmt.Sscanf(strings.TrimSpace(part[i+1:]), "%g", &sec); err == nil && sec > 0 {
				e.dur = time.Duration(sec * float64(time.Second))
			}
		}
		if e.name != "" {
			out = append(out, e)
		}
	}
	return out
}

// showFile is the JSON compose-file: a timeline of scenes, each with its own
// length and animation speed. Written by the timeline editor.
type showFile struct {
	SceneMS int `json:"sceneMs"`
	Show    []struct {
		Scene   string  `json:"scene"`
		Seconds float64 `json:"seconds"`
		Speed   float64 `json:"speed"`
	} `json:"show"`
}

// loadShow reads a JSON compose-file into show steps.
func loadShow(path string) ([]showStep, int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var sf showFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return nil, 0, fmt.Errorf("%s: %w", path, err)
	}
	var out []showStep
	for _, s := range sf.Show {
		if s.Scene == "" {
			continue
		}
		e := showStep{name: s.Scene, speed: s.Speed}
		if e.speed <= 0 {
			e.speed = 1
		}
		if s.Seconds > 0 {
			e.dur = time.Duration(s.Seconds * float64(time.Second))
		}
		out = append(out, e)
	}
	return out, sf.SceneMS, nil
}

// demoSceneFilter, when set, restricts the demo to the one scene of that name,
// looping it — so a single effect can be watched and iterated on in isolation.
var demoSceneFilter string
var recolorOn bool

// recolorIdentity, when set, makes maybeRecolor rewrite each ALV cell with the
// colour it already has. That runs the full decode→recompress→re-chunk pipeline
// while changing nothing, so a live GUI test isolates whether our recompression
// alone (not the colour values) is what a real GUI rejects.
var recolorIdentity bool

// recolorPassthrough, when set, makes maybeRecolor re-encode a grid frame with
// Compress=0 but leave the ALV blob byte-identical — no recolour at all. It
// isolates whether the live GUI stalls on our uncompressed DIAG re-encode itself
// (independent of any ALV recompression).
var recolorPassthrough bool

// maybeRecolor patches the colours of any ALV grid a frame carries with our own
// pattern, so a replayed grid shows the colours we choose. It returns nil when
// the frame has no decodable grid, leaving it untouched.
func maybeRecolor(data []byte, log func(string, ...any)) []byte {
	m, err := diag.ParseMessage(data, false)
	if err != nil {
		return nil
	}
	items := diag.ParseItems(m.Body)
	changed := false
	for i, it := range items {
		if it.ID != 0x08 { // RFC_TR
			continue
		}
		g, e := alv.DecodeGrid(it.Value)
		if e != nil || len(g.Rows) == 0 {
			continue
		}
		if recolorPassthrough {
			// Leave the blob untouched; just mark the frame so it is re-encoded
			// uncompressed. This tells us if the Compress=0 re-frame alone stalls.
			changed = true
			log("passthrough grid frame (%d cols, %d rows), blob unchanged", len(g.Cols), len(g.Rows))
			continue
		}
		fn := func(row, col int) int {
			// A distinctive diagonal, clearly ours, so a live test is unambiguous.
			return alv.ColourField((row*2+col*3)%7+1, true, false)
		}
		if recolorIdentity {
			// Rewrite each cell with its own colour: the pipeline runs, nothing
			// changes, so a live test blames the recompression, not the colours.
			fn = func(row, col int) int { return g.Colours[row][col] }
		}
		patched, pe := alv.PatchColours(it.Value, fn)
		if pe == nil {
			items[i].Value = patched
			changed = true
			log("recoloured an ALV grid (%d cols, %d rows)", len(g.Cols), len(g.Rows))
		}
	}
	if !changed {
		return nil
	}
	h := m.Header
	h.Compress = 0
	out, err := diag.EncodeMessage(h, items, false)
	if err != nil {
		return nil
	}
	return out
}

func main() {
	listen := flag.String("listen", ":3200", "address SAP GUI connects to (instance NN = port 32NN)")
	capture := flag.String("capture", "assets/probe.scrubbed.jsonl", "tap capture to replay (scrubbed asset)")
	conn := flag.Int("conn", 1, "connection of the capture to replay")
	mode := flag.String("mode", "demo", "logon | menu | counter | widgets | anim | demo | colorlist | ...")
	menuAt := flag.Int("menu-at", 2, "client frame index whose replies are the start menu (mode menu, counter)")
	screenFrame := flag.Int("screen", 209, "server frame index that shows the probe's screen (mode counter)")
	pushFrame := flag.Int("push", 222, "server frame index the pushed counter frames are made from (mode counter)")
	pushMS := flag.Int("push-ms", 300, "cadence of the pushed frames")
	msgType := flag.String("msg-type", "E", "status message to trigger a sound under the animation: S, W, E or I (empty = none)")
	msgLoop := flag.Int("msg-loop", 0, "re-send the sound every N frames (0 = once, on the first frame)")
	sceneMS := flag.Int("scene-ms", 3000, "how long each scene of the demo mode runs, in milliseconds (wall clock, not frames)")
	scene := flag.String("scene", "", "demo mode: play only this one scene, looping (e.g. fireworks, helix, equalizer, matrix)")
	playlist := flag.String("playlist", "", "demo mode: compose a show as a comma list of scene[:seconds] entries, played in order and looped, e.g. \"orbit:6,tornado:10,solid:8,fireworks:9\" (seconds default to -scene-ms)")
	show := flag.String("show", "assets/show.json", "demo mode: load a JSON compose-file (timeline of scenes with seconds and speed)")
	httpAddr := flag.String("http", "", "serve the live timeline composer on this addr (e.g. :8088): edit the show in a browser, it saves to the -show file, and the next SAP GUI to connect plays it")
	recolor := flag.Bool("recolor", false, "patch the colours of any ALV grid in a replayed frame with our own pattern")
	recolorId := flag.Bool("recolor-identity", false, "recolor pipeline runs but writes each cell its existing colour (isolates recompression from colour values)")
	recolorStored := flag.Bool("recolor-stored", false, "compress recoloured ALV blobs with DEFLATE stored blocks instead of dynamic Huffman")
	recolorPass := flag.Bool("reencode", false, "re-encode grid frames with Compress=0 but leave the ALV blob byte-identical (isolates the DIAG re-encode)")
	flag.Parse()

	capturePath = *capture
	demoSceneMS = *sceneMS
	demoSceneFilter = *scene
	if *show != "" {
		showPath = *show
		if _, statErr := os.Stat(*show); statErr == nil {
			steps, sms, err := loadShow(*show)
			if err != nil {
				fmt.Fprintln(os.Stderr, "server: show:", err)
				os.Exit(1)
			}
			demoShow = steps
			if sms > 0 {
				demoSceneMS = sms
			}
		} else if *httpAddr == "" {
			// No file and no composer to create one — a genuine mistake.
			fmt.Fprintln(os.Stderr, "server: show:", statErr)
			os.Exit(1)
		}
		// else: -http is on; the composer will write this file. Start with an
		// empty show (play all scenes) until it does.
	} else {
		demoShow = parsePlaylist(*playlist)
	}
	if *httpAddr != "" {
		// The composer needs a file to write; default it when -show was omitted.
		if showPath == "" {
			showPath = "show.json"
		}
		go serveComposer(*httpAddr, showPath, func(f string, a ...any) {
			fmt.Fprintf(os.Stderr, "composer: "+f+"\n", a...)
		})
	}
	recolorOn = *recolor || *recolorId || *recolorPass
	recolorIdentity = *recolorId
	recolorPassthrough = *recolorPass
	if *recolorStored {
		alv.CompressLevel = flate.NoCompression
	}
	cap, err := replay.Load(*capture, *conn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "server: %d client frames, %d server frames on connection %d; mode %s; listening on %s\n", len(cap.Client), len(cap.Server), *conn, *mode, *listen)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintln(os.Stderr, "server: accept:", err)
			continue
		}
		cad := time.Duration(*pushMS) * time.Millisecond
		if *mode == "anim" || *mode == "demo" {
			if *pushMS == 300 {
				cad = 80 * time.Millisecond // faster default for the full-screen effect
			}
			if cad < 60*time.Millisecond {
				cad = 60 * time.Millisecond // a floor: the GUI cannot consume a full-screen frame faster
			}
		}
		if *mode == "widgets" && *pushMS == 300 {
			cad = 60 * time.Millisecond // light frames, so a brisker default
		}
		if *mode == "iconanim" || *mode == "led" {
			if *pushMS == 300 {
				cad = 180 * time.Millisecond // gentle default for a pushed list
			}
			if cad < 80*time.Millisecond {
				cad = 80 * time.Millisecond
			}
		}
		go serve(ctx, c, cap, *mode, *menuAt, *screenFrame, *pushFrame, cad, msgByte(*msgType), *msgLoop)
	}
}

// findCounterFrames locates the probe's screen in the capture by content
// rather than a fixed index, since a growing capture shifts the numbers:
// the first server frame whose DYNT_ATOM names GV_TICKS is the screen, the
// last is a steady counter frame to push from.

// findSelectionFrame locates a captured selection screen by content: the
// first server frame whose DYNT_ATOM has a field named P_MS. Its input
// fields are in the dynpro definition, so the client submits what the user
// types into them.
func findSelectionFrame(cap *replay.Capture) (int, bool) {
	for _, f := range cap.Server {
		m, err := diag.ParseMessage(f.Data, false)
		if err != nil {
			continue
		}
		for _, it := range diag.ParseItems(m.Body) {
			if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
				if _, byName := diag.FieldIndex(it.Value); byName["P_MS"] != 0 || hasKey(byName, "P_MS") {
					return f.Index, true
				}
			}
		}
	}
	return 0, false
}

func hasKey(m map[string]int, k string) bool { _, ok := m[k]; return ok }

// inputRespond reads what the user typed into P_MS and shows it back with
// its square, reusing the real selection screen's atoms so its dynpro
// definition still matches and the client still submits. This is data in
// and data out, our Go code the PBO and PAI.
func inputRespond(cap *replay.Capture, selFrame int, client []diag.FieldValue, st *appState, log func(string, ...any)) []byte {
	f, ok := cap.ServerFrame(selFrame)
	if !ok {
		return nil
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		return nil
	}
	items := diag.ParseItems(m.Body)
	for i, it := range items {
		if it.Type != diag.ItemAPPL4 || it.ID != 0x09 || it.SID != 0x02 {
			continue
		}
		atoms, byName := diag.FieldIndex(it.Value)
		// Read what the user left in P_MS: the client echoes it at the cell
		// the server placed the field. Log every value the client returned,
		// so a miss is visible.
		typed := ""
		if idx, ok := byName["P_MS"]; ok {
			ms := atoms[idx]
			for _, fv := range client {
				if fv.Row == ms.Row && fv.Col == ms.Col {
					typed = strings.TrimSpace(fv.Value)
				}
			}
		}
		log("input turn %d: client returned %d field(s), P_MS=%q", st.turns, len(client), typed)
		// The number to work from: what the user typed if it parses, else
		// what we last showed. Then P_MS = P_MS + 1, shown back.
		n := st.lastMS
		if v, perr := strconv.Atoi(typed); perr == nil {
			n = v
		}
		n++
		st.lastMS = n
		diag.SetField(atoms, byName, "P_MS", strconv.Itoa(n))
		diag.SetField(atoms, byName, "P_TICKS", strconv.Itoa(n))
		diag.SetField(atoms, byName, "P_TIME", fmt.Sprintf("turn %d", st.turns))
		diag.SetField(atoms, byName, "P_BAR", fmt.Sprintf("Go did P_MS+1 -> %d", n))
		items[i].Value = diag.EncodeDyntAtoms(atoms)
		h := m.Header
		h.Compress = 0
		out, err := diag.EncodeMessage(h, items, false)
		if err != nil {
			return nil
		}
		return out
	}
	return nil
}

func findCounterFrames(cap *replay.Capture) (screen, pushIdx int, ok bool) {
	first, last := -1, -1
	for _, f := range cap.Server {
		m, err := diag.ParseMessage(f.Data, false)
		if err != nil {
			continue
		}
		for _, it := range diag.ParseItems(m.Body) {
			if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 && strings.Contains(string(it.Value), "GV_TICKS") {
				if first < 0 {
					first = f.Index
				}
				last = f.Index
			}
		}
	}
	if first < 0 {
		return 0, 0, false
	}
	return first, last, true
}

func serve(ctx context.Context, c net.Conn, cap *replay.Capture, mode string, menuAt, screenFrame, pushFrame int, cadence time.Duration, msgType byte, msgLoop int) {
	defer c.Close()
	log := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "[%s] "+format+"\n", append([]any{c.RemoteAddr()}, a...)...)
	}
	log("connected")
	send := func(what string, data []byte) error {
		if recolorOn {
			if p := maybeRecolor(data, log); p != nil {
				data = p
				what += " [recoloured]"
			}
		}
		plain, err := replay.Plain(data)
		if err != nil {
			log("%s: cannot flatten (%v); sending as captured", what, err)
			plain = data
		}
		frame, err := ni.EncodeFrame(plain)
		if err != nil {
			return err
		}
		_, err = c.Write(frame)
		log("-> %s, %d bytes", what, len(plain))
		return err
	}
	// closeSession ends the dialog the way the real server does: a bare
	// DIAG header with the end-of-conversation and end-of-program flags,
	// no body. The GUI closes the window on it, where a dropped socket
	// gave a "connection broken" error instead.
	closeSession := func() {
		h := diag.Header{ComFlag: diag.FlagTermEOC | diag.FlagTermEOP, MsgInfo: 0x01}
		if fr, err := ni.EncodeFrame(h.Bytes()); err == nil {
			_, _ = c.Write(fr)
		}
		log("-> session end (EOP)")
	}
	dec, _ := ni.NewFrameDecoder(64 << 20)
	buf := make([]byte, 64<<10)
	// group is the client frame of the capture the next reply group belongs to.
	group := 0
	// clientFrames counts the data (non-NI) frames this connection has sent.
	// The 200-byte DP header rides only the very first one (the INI frame),
	// so only that frame is parsed with the DP-header skip — a live client's
	// later frames carry no DP header, whatever replay group we are on.
	clientFrames := 0
	if mode == "menu" || mode == "counter" {
		group = menuAt
	}
	st := &appState{}
	popup := replay.FindPopup(capturePath)
	var listWrap []byte
	if mode == "colorlist" || mode == "iconanim" || mode == "led" || mode == "demo" {
		listWrap = replay.FindListWrap(capturePath)
		if listWrap != nil {
			log("plain list wrapper located in the capture")
		}
	}
	jokeStep := 0
	animOn := false
	animCadence := cadence
	animMsgType = msgType
	animMsgLoop = msgLoop
	selFrame, selOK := 0, false
	if mode == "input" {
		selFrame, selOK = findSelectionFrame(cap)
		if selOK {
			log("selection screen located by content: server frame #%d", selFrame)
		}
	}
	if mode == "counter" || mode == "flash" || mode == "synth" || mode == "list" || mode == "app" || mode == "showcase" || mode == "states" || mode == "anim" || mode == "widgets" || mode == "demo" || mode == "snake" || mode == "echo" {
		if sf, pf, ok := findCounterFrames(cap); ok {
			screenFrame, pushFrame = sf, pf
			log("counter screen located by content: screen #%d, push #%d", sf, pf)
		}
	}
	var pushing chan struct{}
	var game *snakeGame
	for {
		n, err := c.Read(buf)
		if err != nil {
			log("closed: %v", err)
			return
		}
		frames, err := dec.Push(buf[:n])
		if err != nil {
			log("ni: %v", err)
			return
		}
		for _, payload := range frames {
			if name, ok := diag.NIControl(payload); ok {
				log("<- %s", name)
				if name == "NI_PING" {
					_ = send("NI_PONG", []byte("NI_PONG\x00"))
				}
				continue
			}
			hasDP := clientFrames == 0 && len(payload) > diag.DPHeaderLen
			clientFrames++
			m, perr := diag.ParseMessage(payload, hasDP)
			if perr == nil {
				log("<- client frame, %d bytes, %s, %d items", len(payload), m.Header, len(diag.ParseItems(m.Body)))
			} else {
				log("<- client frame, %d bytes (%v)", len(payload), perr)
			}
			// An exit command — window-close /i, /n, Back, exit. Answer with
			// the joke popup once, then accept the next click (any button) by
			// closing. Any of these gets the viewer out of the show.
			if perr == nil && jokeStep == 0 && isExitCmd(diag.ParseItems(m.Body)) {
				// Stop any animation first: a running push loop would otherwise
				// keep drawing over the log-off popup, and keep writing after the
				// session ends. This is the /i reaction every mode now shares.
				if pushing != nil {
					close(pushing)
					pushing = nil
				}
				if jp := jokePopup1(popup); jp != nil {
					jokeStep = 1
					_ = send("joke popup 1: Where are you going???", jp)
					continue
				}
				closeSession()
				log("window close accepted (no popup template)")
				return
			}
			if jokeStep == 1 {
				if jp := jokePopup2(popup); jp != nil {
					jokeStep = 2
					_ = send("joke popup 2: =(", jp)
					continue
				}
				closeSession()
				return
			}
			if jokeStep == 2 {
				closeSession()
				log("closing after the two jokes")
				return
			}
			// /o (new window) turns the current window into an animation.
			if perr == nil && animOn == false && isNewWindow(diag.ParseItems(m.Body)) {
				animOn = true
				log("/o: starting animation in this window")
				go push(ctx, c, animRenderer(cap, screenFrame, log), animCadence, pushing, log)
				continue
			}
			if animOn {
				continue
			}
			if mode == "input" {
				var client []diag.FieldValue
				if perr == nil {
					cItems := diag.ParseItems(m.Body)
					client = diag.ClientFields(cItems)
					var keys []string
					for _, it := range cItems {
						k := it.Key()
						if it.Type == diag.ItemAPPL || it.Type == diag.ItemAPPL4 {
							k = fmt.Sprintf("%s(%d)", k, len(it.Value))
						}
						keys = append(keys, k)
					}
					log("client items: %s", strings.Join(keys, " "))
				}
				if selOK {
					st.turns++
					if out := inputRespond(cap, selFrame, client, st, log); out != nil {
						_ = send("selection screen with the answer", out)
					}
				}
				continue
			}
			if mode == "app" {
				st.turns++
				if perr == nil {
					st.fields = diag.ClientFields(diag.ParseItems(m.Body))
					st.events = diag.Events(diag.ParseItems(m.Body))
				}
				if out, ok := appRespond(cap, screenFrame, st); ok {
					_ = send(fmt.Sprintf("app screen, turn %d", st.turns), out)
				}
				continue
			}
			if mode == "echo" {
				// No buttons: every client frame is a keypress or menu action.
				// The function code is not always in VARINFO.04 — a key can
				// arrive as a UI_EVENT — so show the VALUES of the items that
				// carry an action, not just their keys, to read what each key
				// actually sent.
				st.turns++
				if perr == nil {
					items := diag.ParseItems(m.Body)
					fc := funcCode(items)
					changed := echoDiff(st, items)
					// The interesting part first: the function code, then what
					// changed since the last press. Constant housekeeping is
					// dropped, so a key that actually differs stands out.
					head := "same"
					if changed != "" {
						head = changed
					}
					line := fmt.Sprintf("#%d fc=%q  %s", st.turns, fc, head)
					st.echo = append(st.echo, line)
					if len(st.echo) > 17 {
						st.echo = st.echo[len(st.echo)-17:]
					}
					log("echo #%d fc=%q com=%02x type=%02x  changed:{%s}  all:[%s]",
						st.turns, fc, m.Header.ComFlag, m.Header.MsgType, changed, itemVals(items))
				}
				if out, ok := echoRespond(cap, screenFrame, st); ok {
					_ = send("echo screen", out)
				}
				continue
			}
			if mode == "snake" {
				// The first client frame starts the game and a step loop; every
				// later frame is a button press feeding the game a direction.
				// Unlike the animations, a keypress does NOT stop it — it steers.
				if game == nil {
					game = newSnakeGame(48, 16)
					_ = send("snake: first frame", staticRespondWrap(cap, screenFrame, game.render()))
					pushing = make(chan struct{})
					go func(stop <-chan struct{}) {
						t := time.NewTicker(cadence)
						defer t.Stop()
						for {
							select {
							case <-ctx.Done():
								return
							case <-stop:
								return
							case <-t.C:
							}
							game.step()
							fr, err := ni.EncodeFrame(staticRespondWrap(cap, screenFrame, game.render()))
							if err != nil {
								return
							}
							if _, err := c.Write(fr); err != nil {
								log("snake push: %v", err)
								return
							}
						}
					}(pushing)
					continue
				}
				if perr == nil {
					items := diag.ParseItems(m.Body)
					fc := funcCode(items)
					log("snake input: fcode=%q items=[%s]", fc, itemKeys(items))
					game.input(fc)
				}
				continue
			}
			if mode == "states" || mode == "showcase" {
				var client []diag.FieldValue
				if perr == nil {
					client = diag.ClientFields(diag.ParseItems(m.Body))
				}
				if out := staticRespond(cap, screenFrame, mode, client); out != nil {
					_ = send("static screen (input preserved)", out)
				}
				continue
			}
			if (mode == "flash" || mode == "synth" || mode == "list" || mode == "anim" || mode == "colorlist" || mode == "widgets" || mode == "demo" || mode == "iconanim" || mode == "led") && pushing == nil {
				rend := renderer(mode, cap, screenFrame, pushFrame, listWrap, log)
				if mode == "flash" {
					// flash replays the captured screen as it was.
					if f, ok := cap.ServerFrame(screenFrame); ok {
						_ = send(fmt.Sprintf("the probe's screen, no handshake (capture S->C #%d)", screenFrame), f.Data)
					}
				} else {
					// synth and the static demos draw their own first frame.
					_ = send("our own screen, no handshake", rend(0))
				}
				pushing = make(chan struct{})
				// A static screen is sent once and left alone; only the
				// animated modes keep pushing on a timer. Pushing a static
				// screen every tick overwrote what the user was typing.
				if mode == "list" || mode == "colorlist" {
					// a list is static: sent once, no timer.
				} else {
					go push(ctx, c, rend, cadence, pushing, log)
				}
				continue
			}
			if (mode == "anim" || mode == "widgets" || mode == "demo" || mode == "iconanim" || mode == "led" || animOn) && pushing != nil {
				// A real interaction stops the show; but the GUI also sends frames
				// on its own — notably an ack when the demo switches to the LED list
				// channel (matrix -> plasma) — and those must NOT stop it. Treat a
				// frame as an interaction only if it carries a function code or a
				// control event; ignore benign/unparseable acks and keep pushing.
				if perr != nil {
					continue
				}
				it := diag.ParseItems(m.Body)
				if funcCode(it) == "" && len(diag.Events(it)) == 0 {
					continue // benign ack (e.g. the list-channel switch) — keep playing
				}
				close(pushing)
				pushing = nil
				animOn = false
				log("animation stopped by the user (fc=%q, %d events)", funcCode(it), len(diag.Events(it)))
				// A keypress during the show — Back, Exit, Cancel, Enter, whatever
				// the viewer reached for — means "get me out". Give it the same
				// two-joke send-off as an explicit /i or /n, so every exit route
				// lands on the joke popup, not a dead "stopped" screen. The
				// list-channel icon animation has no dynpro to draw a popup over,
				// so it just freezes on its last frame.
				if mode != "iconanim" && mode != "led" {
					if jp := jokePopup1(popup); jp != nil {
						jokeStep = 1
						_ = send("joke popup 1: Where are you going??? (keypress)", jp)
						continue
					}
					frozen := frame.New(27, 120).
						Text(1, 2, "animation stopped").
						Text(3, 2, "close the window to exit")
					if out := staticRespondWrap(cap, screenFrame, frozen); out != nil {
						_ = send("animation stopped", out)
					}
				}
				continue
			}
			if pushing != nil {
				// A static screen or a pushing animation. Log what the client
				// sent — its events and any function code — so a window-close
				// signal is visible in the trace.
				if perr == nil {
					ci := diag.ParseItems(m.Body)
					if evs := diag.Events(ci); len(evs) > 0 {
						log("client events: %+v (com=%02x)", evs, m.Header.ComFlag)
					} else {
						log("client frame: %d items, com=%02x type=%02x", len(ci), m.Header.ComFlag, m.Header.MsgType)
					}
				}
				continue
			}
			if group >= len(cap.Replies) {
				log("no captured reply for client frame %d; silent", group)
				continue
			}
			for i, f := range cap.Replies[group] {
				if err := send(fmt.Sprintf("reply %d/%d to client frame %d (capture S->C #%d)", i+1, len(cap.Replies[group]), group, f.Index), f.Data); err != nil {
					log("write: %v", err)
					return
				}
			}
			group++
			if mode == "counter" && group == menuAt+3 {
				// The start menu is up. Show the probe's screen, then push.
				if f, ok := cap.ServerFrame(screenFrame); ok {
					_ = send(fmt.Sprintf("the probe's screen (capture S->C #%d)", screenFrame), f.Data)
				}
				pushing = make(chan struct{})
				go push(ctx, c, renderer(mode, cap, screenFrame, pushFrame, listWrap, log), cadence, pushing, log)
			}
		}
	}
}

var counterText = regexp.MustCompile(`\x20{4,9}[0-9]{1,6}\x20`)

// push sends the probe's pushed frame again and again, the counter in it
// replaced, the header's stat=f0 kept as the capture had it.
// renderer picks how the pushed frames are built: synth from our own
// frame.Screen, or a patch of a captured frame.
// statesScreen shows an input field in each state we can set: active,
// protected (inactive), hidden, and value-help (F4). The hidden one is
// there but not drawn; the F4 one shows the matchcode button.
func statesScreen() *frame.Screen {
	return frame.New(27, 120).
		Frame(0, 0, 70, 14, "Input field states and types").
		Text(1, 2, "active").Input(1, 20, 20, "S_ACT", "type here").
		Text(2, 2, "inactive").InputProtected(2, 20, 20, "S_INA", "cannot edit").
		Text(3, 2, "hidden").InputHidden(3, 20, 20, "S_HID", "secret").
		Text(3, 44, "(hidden is here, not shown)").
		Text(4, 2, "F4 help").InputF4(4, 20, 20, "S_F4", "press F4").
		Text(6, 2, "date").Date(6, 20, "S_DAT", "2026-09-09").
		Text(7, 2, "time").Time(7, 20, "S_TIM", "14:30:00").
		Text(9, 2, "static screen: type freely, it will not be overwritten")
}

// statesRenderer wraps the located screen frame and swaps in the states screen.
func statesRenderer(cap *replay.Capture, wrapFrame int, log func(string, ...any)) func(n int) []byte {
	f, ok := cap.ServerFrame(wrapFrame)
	if !ok {
		return func(int) []byte { return nil }
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		return func(int) []byte { return nil }
	}
	items := diag.ParseItems(m.Body)
	for i, it := range items {
		if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
			items[i].Value = statesScreen().Encode()
			h := m.Header
			h.Compress = 0
			out, err := diag.EncodeMessage(h, items, false)
			if err != nil {
				return func(int) []byte { return nil }
			}
			return func(int) []byte { return out }
		}
	}
	return func(int) []byte { return nil }
}

// isClose reports whether a client frame is the window-close request: the
// GUI sends the system command "/i" in a VARINFO.04 item when the user
// shuts the window.
// isNewWindow reports whether the client sent the /o system command, the
// one that opens a new session window.
func isNewWindow(items []diag.Item) bool {
	for _, it := range items {
		if it.Type == diag.ItemAPPL && it.ID == 0x0c && it.SID == 0x04 && strings.HasPrefix(strings.TrimSpace(string(it.Value)), "/o") {
			return true
		}
	}
	return false
}

// funcCode is the function code the client sent in its OK-code field
// (VARINFO.04) — a pushbutton's "=UP", a system command, or empty.
func funcCode(items []diag.Item) string {
	for _, it := range items {
		if it.Type == diag.ItemAPPL && it.ID == 0x0c && it.SID == 0x04 {
			return strings.TrimSpace(string(it.Value))
		}
	}
	return ""
}

// itemKeys is a one-line list of a frame's item keys, for the log.
func itemKeys(items []diag.Item) string {
	ks := make([]string, 0, len(items))
	for _, it := range items {
		ks = append(ks, it.Key())
	}
	return strings.Join(ks, " ")
}

// itemVals renders the values of the items that can carry an action — the
// OK-code, the UI events, the function-info varinfos — as key="text"|hex, so a
// key's identity (which arrives as a named UI event, not always a function
// code) is visible. Housekeeping items (session, user, system) are skipped.
func itemVals(items []diag.Item) string {
	var events, rest []string
	for _, it := range items {
		k := it.Key()
		if len(it.Value) == 0 {
			continue
		}
		switch {
		case strings.Contains(k, "UI_EVENT"), strings.Contains(k, "VARINFO.04"):
			// The action channels — the OK-code and the control events — go
			// first, since they are what a keypress or a click carries.
			events = append(events, fmt.Sprintf("%s=%s", k, showVal(it.Value)))
		case strings.Contains(k, "VARINFO.06"), strings.Contains(k, "VARINFO.08"),
			strings.Contains(k, "VARINFO.09"), strings.HasPrefix(k, "APPL DYNT"):
			rest = append(rest, fmt.Sprintf("%s=%s", k, showVal(it.Value)))
		}
	}
	return strings.Join(append(events, rest...), "  ")
}

// echoDiff compares this frame's screen/action items against the last one and
// returns the ones that changed, value included. Housekeeping items (session,
// user, system, RFC) are ignored, so what is left is what a keypress actually
// moved. It updates the stored snapshot in place.
func echoDiff(st *appState, items []diag.Item) string {
	cur := map[string]string{}
	for _, it := range items {
		k := it.Key()
		if strings.HasPrefix(k, "APPL ST_") || strings.HasPrefix(k, "APPL RFC_TR") ||
			k == "SES" || k == "EOM" || k == "CHL" {
			continue
		}
		cur[k] = fmt.Sprintf("%x", it.Value)
	}
	var changed []string
	for _, it := range items { // report in wire order
		k := it.Key()
		v, ok := cur[k]
		if !ok {
			continue
		}
		if st.prev[k] != v {
			changed = append(changed, fmt.Sprintf("%s=%s", k, showVal(it.Value)))
		}
		delete(cur, k) // so a repeated key is reported once
	}
	for k := range st.prev {
		if _, stillHere := indexKey(items, k); !stillHere {
			changed = append(changed, k+"(gone)")
		}
	}
	// Rebuild the snapshot from the frame.
	next := map[string]string{}
	for _, it := range items {
		k := it.Key()
		if strings.HasPrefix(k, "APPL ST_") || strings.HasPrefix(k, "APPL RFC_TR") ||
			k == "SES" || k == "EOM" || k == "CHL" {
			continue
		}
		next[k] = fmt.Sprintf("%x", it.Value)
	}
	st.prev = next
	return strings.Join(changed, "  ")
}

// indexKey reports whether any item carries the given key.
func indexKey(items []diag.Item, key string) (int, bool) {
	for i, it := range items {
		if it.Key() == key {
			return i, true
		}
	}
	return 0, false
}

// showVal is a value as printable text, with a hex tail for a short one so a
// non-printable byte is still readable.
func showVal(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('.')
		}
	}
	s := sb.String()
	if len(b) <= 24 {
		return fmt.Sprintf("%q|%x", s, b)
	}
	return fmt.Sprintf("%q", s)
}

func isClose(items []diag.Item) bool {
	for _, it := range items {
		if it.Type == diag.ItemAPPL && it.ID == 0x0c && it.SID == 0x04 && strings.TrimSpace(string(it.Value)) == "/i" {
			return true
		}
	}
	return false
}

// isExitCmd is any command a viewer uses to leave the show: the window close
// (/i), end-transaction (/n and its variants), Back, and a plain "exit". They
// all get the same two-joke send-off so it is easy to get out of the demo,
// however you reach for the exit.
func isExitCmd(items []diag.Item) bool {
	if isClose(items) { // the window close, /i
		return true
	}
	switch strings.ToLower(funcCode(items)) {
	case "/n", "/nend", "/nex", "/bend", "back", "=back", "exit":
		return true
	}
	return false
}

// jokePopup swaps the text and buttons of the captured modal log-off popup
// for a joke: "Where are you going?" with two buttons that both say No.
// Reusing the captured frame keeps it a real modal dialog box; only the
// DYNT_ATOM changes. Empty when the capture had no popup to borrow.
func popupWith(popup []byte, scr *frame.Screen) []byte {
	if popup == nil {
		return nil
	}
	m, err := diag.ParseMessage(popup, false)
	if err != nil {
		return nil
	}
	items := diag.ParseItems(m.Body)
	for i, it := range items {
		if it.Type != diag.ItemAPPL4 || it.ID != 0x09 || it.SID != 0x02 {
			continue
		}
		items[i].Value = scr.Encode()
		h := m.Header
		h.Compress = 0
		out, err := diag.EncodeMessage(h, items, false)
		if err != nil {
			return nil
		}
		return out
	}
	return nil
}

// jokePopup1 asks where you are going, with two buttons that both say No.
func jokePopup1(popup []byte) []byte {
	return popupWith(popup, frame.New(6, 60).
		Text(1, 7, "Where are you going???").
		Text(2, 7, "FIORI???").
		Button(4, 7, 10, "No", "=NO1").
		Button(4, 20, 10, "No", "=NO2"))
}

// jokePopup2 is the sad face with a single ok.
func jokePopup2(popup []byte) []byte {
	return popupWith(popup, frame.New(6, 60).
		Text(1, 7, "=(").
		Button(3, 7, 10, "ok", "=OK"))
}

// withSound inserts a status message before EOM on the first frame (and
// every animMsgLoop frames) so the GUI plays that type's sound under an
// animation. The type and loop come from the flags via package state.
func withSound(items []diag.Item, n int) []diag.Item {
	if animMsgType == 0 || !(n == 1 || (animMsgLoop > 0 && n%animMsgLoop == 1)) {
		return items
	}
	return insertStatus(items, animMsgType)
}

// insertStatus puts a status message (whose GUI sound is type t) just before
// EOM, so the client plays that sound with the frame.
func insertStatus(items []diag.Item, t byte) []diag.Item {
	msg := diag.StatusMessage(t, "odgp: now playing")
	out := make([]diag.Item, 0, len(items)+1)
	for _, it := range items {
		if it.Type == diag.ItemEOM {
			out = append(out, msg)
		}
		out = append(out, it)
	}
	return out
}

func widgetsScreen(t int) *frame.Screen {
	scr := frame.New(27, 120)
	scr.Frame(0, 0, 78, 24, "OPEN-DIAG-GO-PRO  --  widgets orbiting, drawn by Go")
	// Three buttons on a circle, 120 degrees apart, turning counter-clockwise.
	// The column radius is larger than the row radius because a character
	// cell is about twice as tall as it is wide, so the path reads round.
	const cx, cy, rx, ry = 39.0, 12.0, 28.0, 9.0
	const speed = 0.06 // radians per frame
	labels := []string{"Go", "DIAG", "no ABAP"}
	for i, lab := range labels {
		ang := -float64(t)*speed + float64(i)*(2.0*math.Pi/3.0) // minus = counter-clockwise
		col := int(cx + rx*math.Cos(ang))
		row := int(cy + ry*math.Sin(ang))
		// Depth: 0 at the back (top), 1 at the front (bottom). The button
		// grows with depth, so the nearer ones look bigger, and the caption
		// is centred in the wider box.
		depth := (math.Sin(ang) + 1.0) / 2.0
		w := 6 + int(depth*12.0)    // 6 wide at the back, 18 at the front
		h := 1 + int(depth*2.0+0.5) // 1 row at the back, up to 3 at the front
		scr.ButtonH(row, col, w, h, centre(lab, w-2), fmt.Sprintf("=B%d", i))
	}
	scr.Text(int(cy), int(cx)-3, "( o )")
	scr.Text(25, 2, fmt.Sprintf("frame %d   3 buttons orbiting CCW   F3/Back stops", t))
	return scr
}

// widgetsRenderer wraps the located screen frame and moves the widgets.
func widgetsRenderer(cap *replay.Capture, wrapFrame int, log func(string, ...any)) func(n int) []byte {
	f, ok := cap.ServerFrame(wrapFrame)
	if !ok {
		return func(int) []byte { return nil }
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		return func(int) []byte { return nil }
	}
	items := diag.ParseItems(m.Body)
	atomIdx := -1
	for i, it := range items {
		if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
			atomIdx = i
		}
	}
	if atomIdx < 0 {
		return func(int) []byte { return nil }
	}
	h := m.Header
	h.Compress = 0
	base := items
	return func(n int) []byte {
		items := append([]diag.Item{}, base...)
		items[atomIdx].Value = widgetsScreen(n).Encode()
		items = withSound(items, n)
		out, err := diag.EncodeMessage(h, items, false)
		if err != nil {
			return nil
		}
		return out
	}
}

// logonWrap is the captured logon screen kept as a backdrop: all of its items
// (the real menu bar, the New password status entry, the Information text) plus
// the index of the fields DYNT_ATOM, so the login scene can swap just the
// fields into it and inherit everything else, native.
type logonWrap struct {
	items      []diag.Item
	header     diag.Header
	fieldIdx   int // the DYNT_ATOM with the fields (and the native Information box)
	welcomeIdx int // the DYNT_ATOM with the welcome text, or -1
}

// loadLogonWrap finds the captured logon screen — the server frame whose
// DYNT_ATOM names RSYST-MANDT — and prepares it as that backdrop, noting the
// fields atom and the welcome-text atom so the login scene can keep the still
// frame verbatim, then swap the fields and drop the welcome once it moves.
func loadLogonWrap(cap *replay.Capture, log func(string, ...any)) (*logonWrap, bool) {
	for _, f := range cap.Server {
		m, err := diag.ParseMessage(f.Data, false)
		if err != nil {
			continue
		}
		items := diag.ParseItems(m.Body)
		fieldIdx, welcomeIdx := -1, -1
		for i, it := range items {
			if it.Type != diag.ItemAPPL4 || it.ID != 0x09 || it.SID != 0x02 {
				continue
			}
			v := string(it.Value)
			if strings.Contains(v, "RSYST-MANDT") {
				fieldIdx = i
			} else if strings.Contains(v, "ABAP Cloud") || strings.Contains(v, "INFO_TAB") {
				welcomeIdx = i
			}
		}
		if fieldIdx < 0 {
			continue
		}
		h := m.Header
		h.Compress = 0
		log("logon wrap located: server frame #%d, fields atom #%d, welcome atom #%d, %d items", f.Index, fieldIdx, welcomeIdx, len(items))
		return &logonWrap{items: items, header: h, fieldIdx: fieldIdx, welcomeIdx: welcomeIdx}, true
	}
	return nil, false
}

// loginFieldsScreen is the login scene when it runs inside the real logon
// backdrop: only the fields (and the Information box that lived in the same
// atom) — the menu, status and welcome text come from the capture. The phases
// are the same as sceneLogin: still, a square drift, then orbiting copies.
func loginFieldsScreen(ts float64) *frame.Screen {
	const homeTop, homeLeft = 0, 1
	const dx, dy = 44.0, 11.0
	scr := frame.New(27, 120)
	// No Information box here: the still frame is shown verbatim (native box),
	// and once the fields move the box is meant to be gone.
	switch {
	case ts < 6:
		demo.LoginFields(scr, homeTop, homeLeft, 0)
	case ts < 12:
		f := (ts - 6) / 6 * 4
		seg := int(f)
		fr := f - float64(seg)
		top, left := float64(homeTop), float64(homeLeft)
		switch seg {
		case 0:
			left = homeLeft + fr*dx
		case 1:
			left = homeLeft + dx
			top = homeTop + fr*dy
		case 2:
			left = homeLeft + (1-fr)*dx
			top = homeTop + dy
		default:
			top = homeTop + (1-fr)*dy
		}
		demo.LoginFields(scr, int(top), int(left), 0)
	case ts < 18:
		demo.OrbitLogins(scr, (ts-12)*1.4, 1)
	case ts < 22:
		demo.OrbitLogins(scr, (ts-12)*1.4, 2)
	default:
		demo.OrbitLogins(scr, (ts-12)*1.4, 3)
	}
	return scr
}

// speedAt is the animation-time multiplier for step i, defaulting to 1 when
// none was set (or the slice is short).
func speedAt(speeds []float64, i int) float64 {
	if i >= 0 && i < len(speeds) && speeds[i] > 0 {
		return speeds[i]
	}
	return 1
}

// demoRenderer cycles the scenes on a wall clock: each runs demoSceneMS
// milliseconds, then the next, then back to the first. The renderer ignores
// the frame counter push hands it and reads the real elapsed time, so the
// scenes advance by seconds, not by frames.
func demoRenderer(cap *replay.Capture, wrapFrame int, listWrap []byte, log func(string, ...any)) func(n int) []byte {
	f, ok := cap.ServerFrame(wrapFrame)
	if !ok {
		return func(int) []byte { return nil }
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		return func(int) []byte { return nil }
	}
	items := diag.ParseItems(m.Body)
	atomIdx := -1
	titleIdx := -1 // the dynpro title (APPL 0x0c/0x09, "SAP") in the green bar
	for i, it := range items {
		if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
			atomIdx = i
		}
		if it.Type == diag.ItemAPPL && it.ID == 0x0c && it.SID == 0x09 {
			titleIdx = i
		}
	}
	if atomIdx < 0 {
		return func(int) []byte { return nil }
	}
	h := m.Header
	h.Compress = 0
	base := items
	logon, haveLogon := loadLogonWrap(cap, log)
	// Prepare the list wrapper so the LED scenes can render in the list channel:
	// keep everything but the list stream, remember where the stream goes.
	var listKeep []diag.Item
	var listHdr diag.Header
	listInsertAt := -1
	haveList := false
	if listWrap != nil {
		if lm, lerr := diag.ParseMessage(listWrap, false); lerr == nil {
			for _, it := range diag.ParseItems(lm.Body) {
				isList := it.Type == diag.ItemSBA || it.Type == diag.ItemSFE || it.Type == diag.ItemSLC ||
					(it.Type == diag.ItemAPPL && it.ID == 0x0c && it.SID == 0x0b)
				if isList {
					if listInsertAt < 0 {
						listInsertAt = len(listKeep)
					}
					continue
				}
				listKeep = append(listKeep, it)
			}
			if listInsertAt < 0 {
				listInsertAt = len(listKeep)
			}
			listHdr = lm.Header
			listHdr.Compress = 0
			haveList = true
			log("demo: list wrapper ready for LED scenes")
		}
	}
	scenes := demo.Scenes()
	byName := map[string]demo.Scene{}
	for _, s := range scenes {
		byName[s.Name] = s
	}
	speeds := make([]float64, len(scenes)) // per-scene animation-time multiplier
	for i := range speeds {
		speeds[i] = 1
	}
	liveShow := currentShow() // re-read the compose-file so a new client sees edits
	switch {
	case len(liveShow) > 0:
		// A composed show: play the named scenes in order, each for its step's
		// length and speed, looping the whole list.
		var sc []demo.Scene
		var sp []float64
		var names []string
		for _, e := range liveShow {
			s, ok := byName[e.name]
			if !ok {
				log("demo: show scene %q not found, skipped", e.name)
				continue
			}
			if e.dur > 0 {
				s.Dur, s.DurMul = e.dur, 0
			}
			sc = append(sc, s)
			sp = append(sp, e.speed)
			names = append(names, e.name)
		}
		if len(sc) > 0 {
			scenes, speeds = sc, sp
			log("demo: show of %d scenes: %s", len(sc), strings.Join(names, " -> "))
		} else {
			log("demo: show had no known scenes, playing all")
		}
	case demoSceneFilter != "":
		if s, ok := byName[demoSceneFilter]; ok {
			scenes, speeds = []demo.Scene{s}, []float64{1}
			log("demo: filtered to scene %q", demoSceneFilter)
		} else {
			log("demo: scene %q not found, playing all", demoSceneFilter)
		}
	}
	def := time.Duration(demoSceneMS) * time.Millisecond
	if def <= 0 {
		def = 3 * time.Second
	}
	// Each scene's length: its own if it set one, else the default beat.
	durs := make([]time.Duration, len(scenes))
	var total time.Duration
	for i, s := range scenes {
		d := s.Dur
		if d <= 0 {
			d = def
			if s.DurMul > 0 {
				d = time.Duration(float64(def) * s.DurMul)
			}
		}
		durs[i] = d
		total += d
	}
	var start time.Time
	lastScene := -1
	lastName := ""
	prevTs := 0.0
	return func(n int) []byte {
		if start.IsZero() {
			start = time.Now()
		}
		pos := time.Since(start) % total
		idx, acc := 0, time.Duration(0)
		for i, d := range durs {
			if pos < acc+d {
				idx = i
				break
			}
			acc += d
		}
		ts := (pos - acc).Seconds()
		// A single scene (played via -scene) should spin forever, not reset
		// every scene length — give it the raw elapsed time so its animation is
		// continuous.
		if len(scenes) == 1 {
			ts = time.Since(start).Seconds()
		}
		// The show step's speed multiplier scales the animation time (not the
		// scene's wall-clock length): a faster step just animates quicker.
		ts *= speedAt(speeds, idx)
		// Continuity across a run of the same scene: consecutive steps that name
		// the same scene do not restart its clock. Each earlier same-name step's
		// animated length (its wall length times its own speed) carries forward,
		// so a step that only changes a meta-parameter like speed keeps the scene
		// spinning and just changes its rate — no snap back to the start.
		if len(scenes) > 1 {
			for j := idx - 1; j >= 0 && scenes[j].Name == scenes[idx].Name; j-- {
				ts += durs[j].Seconds() * speedAt(speeds, j)
			}
		}
		if idx != lastScene {
			// Announce, and reset the sound phase, only when the actual scene
			// changes; stepping between two steps of the same scene is a seamless
			// continuation, so the beep phase must not be reset there.
			if scenes[idx].Name != lastName {
				log("scene %d/%d: %s (%s)", idx+1, len(scenes), scenes[idx].Name, scenes[idx].Approach)
				prevTs = ts
			}
			lastScene = idx
			lastName = scenes[idx].Name
		}
		// The LED scenes render in the list channel via the list wrapper. The
		// effect time is quantised to ~180ms steps, so consecutive 80ms ticks
		// produce identical frames and the adaptive push (§push) skips them —
		// the LED runs at its own gentle rate while the dynpro scenes stay fast.
		if eff := demo.LEDEffectIndex(scenes[idx].Name); eff >= 0 && haveList {
			t := float64(int(ts/0.18)) * 0.15
			mine := diag.EncodeListItems(demo.LEDSegmentsEff(eff, t))
			out := append(append(append([]diag.Item{}, listKeep[:listInsertAt]...), mine...), listKeep[listInsertAt:]...)
			msg, err := diag.EncodeMessage(listHdr, out, false)
			if err != nil {
				return nil
			}
			return msg
		}
		// The login scene runs inside the real captured logon frame when we
		// have one: swap just the fields atom, so the menu bar, the New
		// password status entry and the Information text are all the genuine
		// article, and only the fields move. This is the reference login on
		// vm.desude.su:3212 — the native SAPMSYST logon (prefilled ?/* fields,
		// the Information box overflowing its frame the way the real GUI draws
		// it), not a synthesized clean-canvas copy.
		if scenes[idx].Name == "login" && haveLogon {
			out := append([]diag.Item{}, logon.items...)
			// Still: the captured frame verbatim — the real Information box and
			// all. Moving: swap the fields for the flying ones (no box) and
			// clear the welcome text, so the box is gone once it comes alive.
			if ts >= 6 {
				out[logon.fieldIdx].Value = loginFieldsScreen(ts).Encode()
				if logon.welcomeIdx >= 0 {
					out[logon.welcomeIdx].Value = nil
				}
			}
			msg, err := diag.EncodeMessage(logon.header, out, false)
			if err != nil {
				return nil
			}
			return msg
		}
		scr := frame.New(27, 120)
		if scenes[idx].Dynpro != nil {
			scenes[idx].Dynpro(ts, scr)
		} else {
			// An LED (list-channel) scene reached here only because the capture
			// had no list frame to wrap; draw a note instead of panicking.
			scr.Text(2, 2, "LED effect needs the capture's list frame")
		}
		// No on-screen captions — just the effect.
		out := append([]diag.Item{}, base...)
		out[atomIdx].Value = scr.Encode()
		// The greet scenes rename the green title bar from "SAP" to "GREETINGS".
		if titleIdx >= 0 && strings.HasPrefix(scenes[idx].Name, "greet") {
			t := out[titleIdx]
			t.Value = []byte("GREETINGS")
			out[titleIdx] = t
		}
		// Sound: a scene with events (a firework burst) drives its own beeps
		// off the frame's time span; otherwise fall back to the beat / single
		// beep. Event audio is natural and sparse — it marks what happened.
		if sfn := scenes[idx].Sound; sfn != nil && animMsgType != 0 {
			if t := sfn(prevTs, ts); t != 0 {
				out = insertStatus(out, t)
			}
		} else if scenes[idx].Sound == nil {
			out = withSound(out, n)
		}
		prevTs = ts
		msg, err := diag.EncodeMessage(h, out, false)
		if err != nil {
			return nil
		}
		return msg
	}
}

// animScreen is one frame of a timed animation: a scrolling marquee and a
// sine wave of stars that moves with t. Every element is a label, so it
// draws on a real GUI and in the TUI alike. This is the effect-engine
// kernel — each tick writes a fresh screen.
func animScreen(t int) *frame.Screen {
	const w, h = 78, 20
	scr := frame.New(27, 120)
	// A marquee scrolling left across the top.
	banner := "  OPEN-DIAG-GO-PRO  ***  a screen SAP GUI draws, driven by Go  ***"
	line := make([]byte, w)
	for i := 0; i < w; i++ {
		line[i] = banner[(t+i)%len(banner)]
	}
	scr.Text(0, 1, string(line))
	// A sine wave of stars.
	for x := 0; x < w; x++ {
		y := h/2 + int(float64(h/2-1)*math.Sin(float64(x+t)/6.0))
		if y >= 0 && y < h {
			scr.Text(2+y, 1+x, "*")
		}
	}
	scr.Text(24, 1, fmt.Sprintf("frame %d   (press F3/Back or close to stop)", t))
	return scr
}

// animRenderer wraps the located screen frame and animates its DYNT_ATOM.
func animRenderer(cap *replay.Capture, wrapFrame int, log func(string, ...any)) func(n int) []byte {
	f, ok := cap.ServerFrame(wrapFrame)
	if !ok {
		return func(int) []byte { return nil }
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		return func(int) []byte { return nil }
	}
	items := diag.ParseItems(m.Body)
	atomIdx := -1
	for i, it := range items {
		if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
			atomIdx = i
		}
	}
	if atomIdx < 0 {
		return func(int) []byte { return nil }
	}
	h := m.Header
	h.Compress = 0
	base := items
	return func(n int) []byte {
		items := append([]diag.Item{}, base...)
		items[atomIdx].Value = animScreen(n).Encode()
		items = withSound(items, n)
		out, err := diag.EncodeMessage(h, items, false)
		if err != nil {
			return nil
		}
		return out
	}
}

// animRenderer's message type/loop are closed over from serve via package
// state set below.
func msgByte(s string) byte {
	if s == "" {
		return 0
	}
	return s[0]
}

// staticRespondWrap wraps a screen in the located screen frame, for a
// one-off static send such as a frozen animation.
func staticRespondWrap(cap *replay.Capture, wrapFrame int, scr *frame.Screen) []byte {
	f, ok := cap.ServerFrame(wrapFrame)
	if !ok {
		return nil
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		return nil
	}
	items := diag.ParseItems(m.Body)
	for i, it := range items {
		if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
			items[i].Value = scr.Encode()
			h := m.Header
			h.Compress = 0
			out, _ := diag.EncodeMessage(h, items, false)
			return out
		}
	}
	return nil
}

// findListFrame locates a captured classic-list frame by content: a server
// frame that carries list segments (the SBA/SFE/SLC/VARINFO.0b stream) — the
// list viewer's shell, which a colourful list of ours reuses.
func findListFrame(cap *replay.Capture) (int, bool) {
	// Prefer a plain WRITE list (our ZODGP_LIST, marked by "end of list"): it
	// carries no controls, so its wrapper does not drag an SE16 settings
	// dialog along the way a data-browser list would. Fall back to any list.
	best, ok := 0, false
	for _, f := range cap.Server {
		m, err := diag.ParseMessage(f.Data, false)
		if err != nil {
			continue
		}
		items := diag.ParseItems(m.Body)
		if !diag.HasListSegments(items) || len(diag.ParseListItems(items)) <= 5 {
			continue
		}
		if !ok {
			best, ok = f.Index, true
		}
		for _, seg := range diag.ParseListItems(items) {
			if strings.Contains(strings.ToLower(seg.Text), "end of list") {
				return f.Index, true
			}
		}
	}
	return best, ok
}

// colourListSegments is the demo list: a heading, a rule, and rows in the
// list colours, so the whole palette shows at once.
func colourListSegments() []diag.ListSegment {
	segs := []diag.ListSegment{
		diag.ListText(0, 2, diag.ColHeading, "OPEN-DIAG-GO-PRO  --  a colourful classic list, drawn by Go"),
		diag.ListText(2, 2, diag.ColHeading, "colour"),
		diag.ListText(2, 20, diag.ColHeading, "sample text"),
	}
	rows := []struct {
		name  string
		color byte
	}{
		{"NORMAL", diag.ColNormal}, {"KEY", diag.ColKey}, {"POSITIVE", diag.ColPositive},
		{"NEGATIVE", diag.ColNegative}, {"TOTAL", diag.ColTotal}, {"GROUP", diag.ColGroup},
	}
	for i, r := range rows {
		row := 4 + i
		segs = append(segs,
			diag.ListText(row, 2, diag.ColNormal, r.name),
			diag.ListText(row, 20, r.color, "the quick brown fox 12345"),
		)
	}
	segs = append(segs, diag.ListText(4+len(rows)+1, 2, diag.ColNormal, "each row uses one FORMAT COLOR; set the colours in your theme"))
	// A row of real icons, drawn by our own bytes: an icon is just the "@XX@"
	// token in a text run, which the list channel turns into a picture.
	iconRow := 4 + len(rows) + 3
	segs = append(segs, diag.ListText(iconRow, 2, diag.ColHeading, "icons, drawn by Go:"))
	icons := []string{diag.IconGreenLight, diag.IconYellowLight, diag.IconRedLight,
		diag.IconLEDGreen, diag.IconChecked, diag.IconOkay, diag.IconCancel}
	for i, ic := range icons {
		segs = append(segs, diag.ListIcon(iconRow, 22+i*3, ic))
	}
	return segs
}

// colorlistRenderer splices a colourful list into the captured list frame:
// its own list stream is dropped and ours put in its place, everything else
// (the env block, the list dynpro, EOM) kept.
func colorlistRenderer(wrap []byte, log func(string, ...any)) func(n int) []byte {
	if wrap == nil {
		log("no plain list frame in the capture to wrap")
		return func(int) []byte { return nil }
	}
	m, err := diag.ParseMessage(wrap, false)
	if err != nil {
		return func(int) []byte { return nil }
	}
	items := diag.ParseItems(m.Body)
	var keep []diag.Item
	insertAt := -1
	for _, it := range items {
		isList := it.Type == diag.ItemSBA || it.Type == diag.ItemSFE || it.Type == diag.ItemSLC ||
			(it.Type == diag.ItemAPPL && it.ID == 0x0c && it.SID == 0x0b)
		if isList {
			if insertAt < 0 {
				insertAt = len(keep)
			}
			continue
		}
		keep = append(keep, it)
	}
	if insertAt < 0 {
		insertAt = len(keep)
	}
	mine := diag.EncodeListItems(colourListSegments())
	final := append(append(append([]diag.Item{}, keep[:insertAt]...), mine...), keep[insertAt:]...)
	h := m.Header
	h.Compress = 0
	payload, err := diag.EncodeMessage(h, final, false)
	if err != nil {
		return func(int) []byte { return nil }
	}
	return func(int) []byte { return payload }
}

// iconAnimSegments is one frame of the icon animation, built from the list
// primitives: a scanner of LEDs sweeping back and forth, a traffic light that
// cycles green-yellow-red on the spot, and a red light bouncing round a box.
// Every glyph is a "@XX@" token in a coloured run placed by (row, col) — a
// moving picture drawn entirely in the list channel.
func iconAnimSegments(n int) []diag.ListSegment {
	var segs []diag.ListSegment
	segs = append(segs, diag.ListText(0, 2, diag.ColHeading, "OPEN-DIAG-GO-PRO  --  animated icons in the list channel"))

	// A KITT scanner: a green LED head with a two-step yellow trail, sweeping a
	// track and bouncing at the ends. Only the lit cells are drawn.
	const track = 22
	pos := triangle(float64(n), track-1)
	for k := 0; k < 3; k++ {
		i := pos - k
		if i < 0 || i >= track {
			continue
		}
		icon := diag.IconYellowLight
		if k == 0 {
			icon = diag.IconLEDGreen
		}
		segs = append(segs, diag.ListIcon(2, 6+i*2, icon))
	}
	segs = append(segs, diag.ListText(3, 6, diag.ColNormal, "scanner (LED head, yellow trail)"))

	// A traffic light cycling on the spot: one lamp lit at a time.
	lights := []string{diag.IconGreenLight, diag.IconYellowLight, diag.IconRedLight}
	segs = append(segs,
		diag.ListText(5, 6, diag.ColNormal, "cycle:"),
		diag.ListIcon(5, 14, lights[(n/6)%3]))

	// A red light bouncing round a box, so motion runs in two dimensions.
	const bw, bh = 30, 8
	bx := triangle(float64(n)*1.3, bw)
	by := triangle(float64(n)*0.7, bh)
	segs = append(segs, diag.ListIcon(7+by, 40+bx, diag.IconRedLight))

	segs = append(segs, diag.ListText(18, 2, diag.ColNormal,
		fmt.Sprintf("frame %d   F3/Back stops   (list-channel push test)", n)))
	return segs
}

// iconanimRenderer splices a fresh icon-animation list into the captured list
// wrapper each frame — the same splice colorlistRenderer does, but rebuilt per
// tick so the icons move. This is the experiment: whether the GUI's list
// processor accepts a server pushing new list frames on a timer.
func iconanimRenderer(wrap []byte, log func(string, ...any)) func(n int) []byte {
	return listPushRenderer(wrap, log, iconAnimSegments)
}

// listPushRenderer splices a fresh list (built by seg for frame n) into the
// captured list wrapper each tick — the shared engine behind every animated
// list-channel effect (icon animation, the LED display). Everything but the
// list stream is kept; ours goes in its place.
func listPushRenderer(wrap []byte, log func(string, ...any), seg func(n int) []diag.ListSegment) func(n int) []byte {
	if wrap == nil {
		log("no plain list frame in the capture to wrap")
		return func(int) []byte { return nil }
	}
	m, err := diag.ParseMessage(wrap, false)
	if err != nil {
		return func(int) []byte { return nil }
	}
	items := diag.ParseItems(m.Body)
	var keep []diag.Item
	insertAt := -1
	for _, it := range items {
		isList := it.Type == diag.ItemSBA || it.Type == diag.ItemSFE || it.Type == diag.ItemSLC ||
			(it.Type == diag.ItemAPPL && it.ID == 0x0c && it.SID == 0x0b)
		if isList {
			if insertAt < 0 {
				insertAt = len(keep)
			}
			continue
		}
		keep = append(keep, it)
	}
	if insertAt < 0 {
		insertAt = len(keep)
	}
	h := m.Header
	h.Compress = 0
	return func(n int) []byte {
		mine := diag.EncodeListItems(seg(n))
		final := append(append(append([]diag.Item{}, keep[:insertAt]...), mine...), keep[insertAt:]...)
		payload, err := diag.EncodeMessage(h, final, false)
		if err != nil {
			return nil
		}
		return payload
	}
}

func ledRenderer(wrap []byte, log func(string, ...any)) func(n int) []byte {
	return listPushRenderer(wrap, log, ledSegments)
}

// staticScreen builds the screen for a static demo mode.
func staticScreen(mode string) *frame.Screen {
	switch mode {
	case "showcase":
		return showcaseScreen()
	case "states":
		return statesScreen()
	}
	return frame.New(24, 80)
}

// staticRespond re-renders a static screen, keeping the values the client
// returned, wrapped in the located screen frame. Answering every PAI this
// way keeps the GUI from hanging on Enter and never loses what was typed.
func staticRespond(cap *replay.Capture, wrapFrame int, mode string, client []diag.FieldValue) []byte {
	f, ok := cap.ServerFrame(wrapFrame)
	if !ok {
		return nil
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		return nil
	}
	items := diag.ParseItems(m.Body)
	for i, it := range items {
		if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
			items[i].Value = staticScreen(mode).Overlay(client).Encode()
			h := m.Header
			h.Compress = 0
			out, err := diag.EncodeMessage(h, items, false)
			if err != nil {
				return nil
			}
			return out
		}
	}
	return nil
}

// showcaseScreen draws one of every element the encoder knows, so a real
// GUI and the TUI both show the whole vocabulary at once.
func showcaseScreen() *frame.Screen {
	return frame.New(27, 120).
		Frame(0, 0, 64, 13, "Field types open-diag-go-pro can encode").
		Text(1, 2, "label").Text(1, 16, "a static caption").
		Text(2, 2, "output").Output(2, 16, 24, "F_OUT", "read-only text", false).
		Text(3, 2, "number").Number(3, 16, 10, "F_NUM", 42).
		Text(4, 2, "input").Input(4, 16, 24, "F_INP", "edit me").
		Text(5, 2, "checkbox").Checkbox(5, 16, "F_CHK", "enabled", true).
		Text(6, 2, "radio").Radio(6, 16, "F_RAD", "option A", true).
		Radio(7, 16, "F_RAD", "option B", false).
		Text(9, 2, "button").Button(9, 16, 16, "Press me", "=GO").
		Text(11, 2, "frame is the box around all of this")
}

// showcaseRenderer wraps the located screen frame and swaps in the showcase.
func showcaseRenderer(cap *replay.Capture, wrapFrame int, log func(string, ...any)) func(n int) []byte {
	f, ok := cap.ServerFrame(wrapFrame)
	if !ok {
		return func(int) []byte { return nil }
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		return func(int) []byte { return nil }
	}
	items := diag.ParseItems(m.Body)
	atomIdx := -1
	for i, it := range items {
		if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
			atomIdx = i
		}
	}
	if atomIdx < 0 {
		return func(int) []byte { return nil }
	}
	items[atomIdx].Value = showcaseScreen().Encode()
	h := m.Header
	h.Compress = 0
	payload, err := diag.EncodeMessage(h, items, false)
	if err != nil {
		return func(int) []byte { return nil }
	}
	return func(int) []byte { return payload }
}

// listRenderer builds a static classic list from frame.Lines and serves
// it in the wrapper of the located screen frame — the write-list shown with
// the primitives we have, no capture of a real list needed.
func listRenderer(cap *replay.Capture, wrapFrame int, log func(string, ...any)) func(n int) []byte {
	f, ok := cap.ServerFrame(wrapFrame)
	if !ok {
		log("no server frame #%d to wrap", wrapFrame)
		return func(int) []byte { return nil }
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		log("wrap frame: %v", err)
		return func(int) []byte { return nil }
	}
	items := diag.ParseItems(m.Body)
	atomIdx := -1
	for i, it := range items {
		if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
			atomIdx = i
		}
	}
	if atomIdx < 0 {
		return func(int) []byte { return nil }
	}
	lines := []string{
		"odgp classic list  --  a write-list, drawn from frame.Lines",
		"------------------------------------------------------------",
		"idx    label      value      square",
		"------------------------------------------------------------",
	}
	for i := 1; i <= 18; i++ {
		lines = append(lines, fmt.Sprintf("%3d    row        %-9d  %d", i, i, i*i))
	}
	lines = append(lines, "------------------------------------------------------------", "end of list")
	scr := frame.New(27, 120).Lines(0, lines)
	items[atomIdx].Value = scr.Encode()
	h := m.Header
	h.Compress = 0
	payload, err := diag.EncodeMessage(h, items, false)
	if err != nil {
		return func(int) []byte { return nil }
	}
	return func(int) []byte { return payload }
}

// appState is what a Go "PBO/PAI" keeps between screens: how many times the
// user acted, and the last values the client returned.
type appState struct {
	turns  int
	lastMS int
	fields []diag.FieldValue
	events []diag.Event
	echo   []string          // the echo server's recent-keypress log
	prev   map[string]string // echo: last frame's item values, for the diff
}

// appScreen is the demo handler: it draws what the user has done. Each PAI
// (an Enter, a button) is one turn; any value the client echoed and any
// control event are shown. This is a Go program's PBO — the screen — built
// from the PAI it just received.
func appScreen(st *appState) *frame.Screen {
	scr := frame.New(27, 120).
		Text(1, 2, "odgp interactive  --  a Go PBO/PAI loop").
		Text(3, 2, "Enters so far").
		Number(3, 20, 10, "GV_TICKS", st.turns).
		Text(5, 2, "press Enter to count; what you type in a field comes back below")
	row := 7
	for _, f := range st.fields {
		if f.Value == "" {
			continue
		}
		scr.Text(row, 2, fmt.Sprintf("field @%d,%d", f.Row, f.Col))
		scr.Text(row, 20, f.Value)
		row++
	}
	for _, e := range st.events {
		scr.Text(row, 2, fmt.Sprintf("event %s/%s %s", e.ShellID, e.EventID, e.Value))
		row++
	}
	return scr
}

// echoScreen shows what the GUI sent on the last few PAIs: the function code
// (a key's or a menu entry's), the header flags, how many control events came,
// and the item keys. It is how we read the keyboard — press a key, see the
// code it produced — with no on-screen buttons at all.
func echoScreen(st *appState) *frame.Screen {
	scr := frame.New(27, 120).
		Frame(0, 0, 110, 24, "odgp echo  --  press keys; this is what SAP GUI sent back").
		Text(2, 2, "PAIs received").
		Number(2, 20, 8, "GV_N", st.turns).
		Text(3, 2, "press function keys, Enter, menu shortcuts (Ctrl/Shift+F..) — most recent first:")
	row := 5
	for i := len(st.echo) - 1; i >= 0 && row < 24; i-- {
		scr.Text(row, 2, st.echo[i])
		row++
	}
	scr.Text(25, 2, "close the window (the [X] / system close sends /i) to exit")
	return scr
}

// echoRespond renders the echo screen into the located screen frame.
func echoRespond(cap *replay.Capture, wrapFrame int, st *appState) ([]byte, bool) {
	out := staticRespondWrap(cap, wrapFrame, echoScreen(st))
	return out, out != nil
}

// appRespond builds one server frame for the app: the demo screen wrapped
// in the located screen frame, its DYNT_ATOM ours.
func appRespond(cap *replay.Capture, wrapFrame int, st *appState) ([]byte, bool) {
	f, ok := cap.ServerFrame(wrapFrame)
	if !ok {
		return nil, false
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		return nil, false
	}
	items := diag.ParseItems(m.Body)
	for i, it := range items {
		if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
			items[i].Value = appScreen(st).Encode()
			h := m.Header
			h.Compress = 0
			out, err := diag.EncodeMessage(h, items, false)
			return out, err == nil
		}
	}
	return nil, false
}

func renderer(mode string, cap *replay.Capture, screenFrame, pushFrame int, listWrap []byte, log func(string, ...any)) func(n int) []byte {
	switch mode {
	case "synth":
		return synthRenderer(cap, pushFrame, log)
	case "list":
		return listRenderer(cap, screenFrame, log)
	case "showcase":
		return showcaseRenderer(cap, screenFrame, log)
	case "states":
		return statesRenderer(cap, screenFrame, log)
	case "anim":
		return animRenderer(cap, screenFrame, log)
	case "colorlist":
		return colorlistRenderer(listWrap, log)
	case "widgets":
		return widgetsRenderer(cap, screenFrame, log)
	case "demo":
		return demoRenderer(cap, screenFrame, listWrap, log)
	case "iconanim":
		return iconanimRenderer(listWrap, log)
	case "led":
		return ledRenderer(listWrap, log)
	}
	return patchRenderer(cap, pushFrame, log)
}

func push(ctx context.Context, c net.Conn, render func(n int) []byte, cadence time.Duration, stop <-chan struct{}, log func(string, ...any)) {
	n := 0
	t := time.NewTicker(cadence)
	defer t.Stop()
	var last []byte // the last frame actually sent, for adaptive skipping
	sent, skipped := 0, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-t.C:
		}
		n++
		// Adaptive rate: render every tick, but only SEND when the frame
		// differs from the last one sent. A static screen (the still logon)
		// is drawn once and then goes quiet; a moving scene sends every frame.
		data := render(n)
		if data == nil {
			continue
		}
		if bytes.Equal(data, last) {
			skipped++
			continue
		}
		frame, err := ni.EncodeFrame(data)
		if err != nil {
			return
		}
		if _, err := c.Write(frame); err != nil {
			log("push: %v", err)
			return
		}
		last = data
		sent++
		if sent%20 == 1 {
			log("-> pushed frame %d (%d bytes; %d sent, %d skipped)", n, len(data), sent, skipped)
		}
	}
}

// patchRenderer reuses a captured push frame and rewrites the counter text
// in its decompressed bytes — the screen is the capture's, only the number
// is ours.
func patchRenderer(cap *replay.Capture, pushFrame int, log func(string, ...any)) func(n int) []byte {
	f, ok := cap.ServerFrame(pushFrame)
	if !ok {
		log("no server frame #%d to push", pushFrame)
		return func(int) []byte { return nil }
	}
	plain, err := replay.Plain(f.Data)
	if err != nil {
		log("push frame: %v", err)
		return func(int) []byte { return nil }
	}
	loc := counterText.FindIndex(plain)
	if loc == nil {
		log("push frame: no counter text found; pushing it unchanged")
	}
	return func(n int) []byte {
		out := append([]byte{}, plain...)
		if loc != nil {
			width := loc[1] - loc[0] - 1
			copy(out[loc[0]:loc[1]], fmt.Sprintf("%*d ", width, n))
		}
		return out
	}
}

// synthRenderer keeps a captured frame only as the wrapper — the env block,
// the dynpro, the DataManager XML — and rebuilds the screen itself each
// tick from a frame.Screen we describe. This is the near side proving
// itself against a real GUI: the number the GUI shows is drawn from our
// own DYNT_ATOM, not the capture's.
func synthRenderer(cap *replay.Capture, pushFrame int, log func(string, ...any)) func(n int) []byte {
	f, ok := cap.ServerFrame(pushFrame)
	if !ok {
		log("no server frame #%d to wrap", pushFrame)
		return func(int) []byte { return nil }
	}
	m, err := diag.ParseMessage(f.Data, false)
	if err != nil {
		log("wrap frame: %v", err)
		return func(int) []byte { return nil }
	}
	items := diag.ParseItems(m.Body)
	atomIdx := -1
	for i, it := range items {
		if it.Type == diag.ItemAPPL4 && it.ID == 0x09 && it.SID == 0x02 {
			atomIdx = i
		}
	}
	if atomIdx < 0 {
		log("wrap frame has no DYNT_ATOM; nothing to synthesize into")
		return func(int) []byte { return nil }
	}
	h := m.Header
	h.Compress = 0
	return func(n int) []byte {
		scr := frame.New(27, 120).
			Text(1, 1, "Ticks").
			Number(1, 9, 10, "GV_TICKS", n)
		items[atomIdx].Value = scr.Encode()
		out, err := diag.EncodeMessage(h, items, false)
		if err != nil {
			return nil
		}
		return out
	}
}
