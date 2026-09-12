package main

import (
	"fmt"
	"math/rand"
	"strings"
	"sync"

	"github.com/oisee/sap-lsd/internal/frame"
)

// snake is the interactive proof: a Go program whose world advances on a
// server timer while the player steers it with on-screen buttons. Each button
// click is one PAI — one function code — so the control model is the discrete,
// one-round-trip-per-press model the DIAG protocol gives, and the timer is
// what makes it feel alive. Drawn in the dynpro channel because that is where
// pushbuttons and a pushed screen live together.

type cell struct{ row, col int }

// snakeGame is the whole game state behind one mutex: the push loop steps and
// renders it, the PAI handler feeds it input, both under the lock.
type snakeGame struct {
	mu         sync.Mutex
	w, h       int
	body       []cell // head at index 0
	dir        cell   // current heading
	pendingDir cell   // the next heading, applied at the next step
	food       cell
	dead       bool
	score      int
	rng        *rand.Rand
}

func newSnakeGame(w, h int) *snakeGame {
	g := &snakeGame{w: w, h: h, rng: rand.New(rand.NewSource(1))}
	g.reset()
	return g
}

func (g *snakeGame) reset() {
	mid := cell{g.h / 2, g.w / 2}
	g.body = []cell{mid, {mid.row, mid.col - 1}, {mid.row, mid.col - 2}}
	g.dir = cell{0, 1} // heading right
	g.pendingDir = g.dir
	g.dead = false
	g.score = 0
	g.placeFood()
}

func (g *snakeGame) placeFood() {
	for {
		f := cell{g.rng.Intn(g.h), g.rng.Intn(g.w)}
		if !g.onBody(f) {
			g.food = f
			return
		}
	}
}

func (g *snakeGame) onBody(c cell) bool {
	for _, b := range g.body {
		if b == c {
			return true
		}
	}
	return false
}

// setDir turns, but never straight back on itself (that would be instant death
// and reads as an unresponsive control).
func (g *snakeGame) setDir(d cell) {
	if d.row == -g.dir.row && d.col == -g.dir.col {
		return
	}
	g.pendingDir = d
}

// input maps a button's function code to a move. Returns a label for the log.
func (g *snakeGame) input(fcode string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch strings.ToUpper(strings.TrimSpace(fcode)) {
	case "=UP":
		g.setDir(cell{-1, 0})
	case "=DOWN":
		g.setDir(cell{1, 0})
	case "=LEFT":
		g.setDir(cell{0, -1})
	case "=RIGHT":
		g.setDir(cell{0, 1})
	case "=NEW":
		g.reset()
	default:
		return fcode
	}
	return fcode
}

// step advances the snake one cell. It dies on a wall or on itself; eating the
// food grows it and drops a new pellet.
func (g *snakeGame) step() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dead {
		return
	}
	g.dir = g.pendingDir
	head := g.body[0]
	next := cell{head.row + g.dir.row, head.col + g.dir.col}
	if next.row < 0 || next.row >= g.h || next.col < 0 || next.col >= g.w {
		g.dead = true
		return
	}
	// Moving onto the current tail is fine — it is about to move away, unless
	// we are also growing.
	grow := next == g.food
	limit := len(g.body)
	if !grow {
		limit-- // the tail vacates
	}
	for i := 0; i < limit; i++ {
		if g.body[i] == next {
			g.dead = true
			return
		}
	}
	g.body = append([]cell{next}, g.body...)
	if grow {
		g.score++
		g.placeFood()
	} else {
		g.body = g.body[:len(g.body)-1]
	}
}

// render draws the current state as a dynpro screen: a framed board with the
// snake and the pellet, the score, the steering buttons, and a game-over line.
func (g *snakeGame) render() *frame.Screen {
	g.mu.Lock()
	defer g.mu.Unlock()
	const top, left = 1, 2 // board origin inside the frame
	scr := frame.New(27, 120)
	scr.Frame(0, 0, g.w+2, g.h+2, "odgp snake  --  drawn by Go, steered by you")
	// The pellet, then the body over it, the head marked apart.
	scr.Text(top+g.food.row, left+g.food.col, "$")
	for i, b := range g.body {
		ch := "o"
		if i == 0 {
			ch = "@"
		}
		scr.Text(top+b.row, left+b.col, ch)
	}
	base := g.h + 3
	scr.Text(base, 2, fmt.Sprintf("score %d", g.score))
	if g.dead {
		scr.Text(base, 14, "GAME OVER  --  press New")
	}
	// The steering buttons: each click is one PAI carrying its function code.
	scr.Button(base+2, 2, 8, "Up", "=UP")
	scr.Button(base+3, 2, 8, "Left", "=LEFT")
	scr.Button(base+3, 12, 8, "Down", "=DOWN")
	scr.Button(base+3, 22, 8, "Right", "=RIGHT")
	scr.Button(base+2, 22, 8, "New", "=NEW")
	scr.Text(base+5, 2, "click a button to steer; the snake moves on the server's timer")
	return scr
}
