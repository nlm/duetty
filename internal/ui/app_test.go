package ui

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/mattn/go-runewidth"
	"github.com/nlm/poly/internal/convo"
)

func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s blocked", what)
	}
}

func TestSetRowsNeverBlocksWithoutRun(t *testing.T) {
	a := New(pair, "m", false)
	within(t, 2*time.Second, "SetRows/SetStatus", func() {
		for i := 0; i < 1000; i++ {
			a.SetRows([]convo.Row{row(1, convo.Done, "fr", "a", "b")})
			a.SetStatus(0.5, "hearing", i, false)
			a.SetNotice("smart turn off")
		}
	})
}

func TestNoticeInStatusLine(t *testing.T) {
	a, sim := newSim(t, 100, 10)
	within(t, 2*time.Second, "SetNotice", func() {
		for i := 0; i < 1000; i++ {
			a.SetNotice("smart turn off · denoise off")
		}
	})
	waitFor(t, "notice drawn", func() bool {
		settle(a)
		return strings.Contains(dump(sim), "smart turn off · denoise off")
	})
	a.SetNotice("")
	waitFor(t, "notice cleared", func() bool {
		settle(a)
		o := dump(sim)
		return !strings.Contains(o, "smart turn off") && strings.Contains(o, "listening")
	})
}

func TestSetRowsNeverBlocksInsideUpdate(t *testing.T) {
	a, sim := newSim(t, 80, 10)
	_ = sim
	res := make(chan bool, 1)
	a.app.QueueUpdate(func() {
		ok := make(chan struct{})
		go func() {
			for i := 0; i < 1000; i++ {
				a.SetRows(nil)
				a.SetStatus(0, "x", 0, false)
			}
			close(ok)
		}()
		// Same goroutine too: a synchronous call from the event loop.
		for i := 0; i < 1000; i++ {
			a.SetRows(nil)
		}
		select {
		case <-ok:
			res <- true
		case <-time.After(2 * time.Second):
			res <- false
		}
	})
	if !<-res {
		t.Fatal("blocked")
	}
	a.Stop()
}

// newSim runs the app on a simulation screen.
func newSim(t *testing.T, w, h int) (*App, tcell.SimulationScreen) {
	return newSimPair(t, pair, w, h)
}

func newSimPair(t *testing.T, p [2]string, w, h int) (*App, tcell.SimulationScreen) {
	t.Helper()
	a := New(p, "mistral-small", false)
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	a.app.SetScreen(sim)
	errc := make(chan error, 1)
	go func() { errc <- a.Run() }()
	t.Cleanup(func() {
		a.Stop()
		select {
		case <-errc:
		case <-time.After(2 * time.Second):
		}
	})
	sizeAndWait(t, a, sim, w, h)
	within(t, 2*time.Second, "start", func() {
		for {
			ch := make(chan struct{})
			a.app.QueueUpdate(func() { close(ch) })
			<-ch
			return
		}
	})
	return a, sim
}

func dump(sim tcell.SimulationScreen) string {
	cells, w, h := sim.GetContents()
	var sb strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 || c.Runes[0] == 0 {
				sb.WriteByte(' ')
				continue
			}
			sb.WriteString(string(c.Runes))
			if len(c.Runes) > 0 && runewidth.RuneWidth(c.Runes[0]) == 2 {
				x++ // skip the continuation cell
			}
		}
		sb.WriteString("|\n")
	}
	return sb.String()
}

func settle(a *App) {
	ch := make(chan struct{})
	a.app.QueueUpdateDraw(func() { close(ch) })
	<-ch
	ch = make(chan struct{})
	a.app.QueueUpdateDraw(func() { close(ch) })
	<-ch
}

var jpRows = []convo.Row{
	row(1, convo.Done, "fr", "Bonjour, je voudrais réserver une table pour ce soir.", "こんにちは、今夜のテーブルを予約したいのですが。"),
	row(2, convo.Done, "ja", "何名様でしょうか？", "How many people?"),
	row(3, convo.Original, "fr", "Deux personnes.", ""),
}

func sizeAndWait(t *testing.T, a *App, sim tcell.SimulationScreen, w, h int) {
	t.Helper()
	// tview re-inits the screen when Run starts, so size after it is up.
	waitFor(t, "app running", func() bool {
		ch := make(chan struct{})
		go a.app.QueueUpdate(func() { close(ch) })
		select {
		case <-ch:
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	})
	sim.SetSize(w, h)
	waitFor(t, "resize", func() bool {
		settle(a)
		cw, ch := sim.Size()
		return cw == w && ch == h && a.sizeIs(w, h)
	})
}

func (a *App) sizeIs(w, h int) bool {
	var ok bool
	ch := make(chan struct{})
	a.app.QueueUpdate(func() { ok = a.width == w && a.height == h; close(ch) })
	<-ch
	return ok
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScreenDump(t *testing.T) {
	for _, w := range []int{80, 50} {
		a, sim := newSimPair(t, [2]string{"fr", "ja"}, w, 14)
		a.SetRows(jpRows)
		a.SetStatus(0.5, "listening", 1, false)
		waitFor(t, "rows drawn", func() bool {
			settle(a)
			o := dump(sim)
			return strings.Contains(o, "working (1)") && strings.Contains(o, "…translating")
		})
		out := dump(sim)
		t.Logf("width %d:\n%s", w, out)
		if !strings.Contains(out, "Français") || !strings.Contains(out, "日本語") {
			t.Fatalf("missing header:\n%s", out)
		}
		// Row 2: the original (right) starts on the same screen line as its translation (left).
		lineJP, lineEN := -1, -1
		for i, ln := range strings.Split(out, "\n") {
			if strings.Contains(ln, "● 何") {
				lineJP = i
			}
			if strings.Contains(ln, "How many") {
				lineEN = i
			}
		}
		if lineJP < 0 || lineJP != lineEN {
			t.Fatalf("row not aligned (%d vs %d):\n%s", lineJP, lineEN, out)
		}
		a.Stop()
		waitDone(t, a)
	}
}

// waitDone waits (bounded) for Run to return and the pump to exit.
func waitDone(t *testing.T, a *App) {
	t.Helper()
	select {
	case <-a.pumpDone:
	case <-time.After(3 * time.Second):
		t.Fatal("pump goroutine did not exit")
	}
}

func TestStopWhileHammeringStatusExitsPump(t *testing.T) {
	for i := 0; i < 20; i++ {
		a, _ := newSim(t, 80, 10)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := 0; ; n++ {
					select {
					case <-stop:
						return
					default:
						a.SetStatus(float64(n%9)/8, "hearing", n%3, false)
					}
				}
			}()
		}
		time.Sleep(20 * time.Millisecond)
		a.Stop()
		waitDone(t, a)
		close(stop)
		wg.Wait()
	}
}

func TestStopBeforeRunReturnsImmediately(t *testing.T) {
	a := New(pair, "m", false)
	a.Stop()
	errc := make(chan error, 1)
	go func() { errc <- a.Run() }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run hung after Stop-before-Run")
	}
	waitDone(t, a)
}

func TestKeys(t *testing.T) {
	a, sim := newSim(t, 80, 14)
	var mu sync.Mutex
	var got []string
	rec := func(s string) { mu.Lock(); got = append(got, s); mu.Unlock() }
	a.OnSubmit(func(s string) { rec("submit:" + s); a.SetRows(jpRows) })
	a.OnCancel(func() { rec("cancel") })
	a.OnMuteToggle(func() { rec("mute") })
	sim.InjectKey(tcell.KeyRune, '?', tcell.ModNone)
	time.Sleep(100 * time.Millisecond)
	settle(a)
	if !strings.Contains(dump(sim), "Ctrl+P") {
		t.Fatalf("help not shown:\n%s", dump(sim))
	}
	sim.InjectKey(tcell.KeyRune, 'x', tcell.ModNone) // closes help
	time.Sleep(50 * time.Millisecond)
	for _, r := range "hi" {
		sim.InjectKey(tcell.KeyRune, r, tcell.ModNone)
	}
	sim.InjectKey(tcell.KeyRune, '?', tcell.ModNone) // input not empty: typed
	sim.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	sim.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	sim.InjectKey(tcell.KeyCtrlP, 0, tcell.ModNone)
	time.Sleep(200 * time.Millisecond)
	settle(a)
	mu.Lock()
	defer mu.Unlock()
	want := "submit:hi?,cancel,mute"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v want %s\n%s", got, want, dump(sim))
	}
}

func TestCtrlCStopsRunReturnsNil(t *testing.T) {
	a := New(pair, "m", false)
	sim := tcell.NewSimulationScreen("UTF-8")
	sim.Init()
	a.app.SetScreen(sim)
	errc := make(chan error, 1)
	go func() { errc <- a.Run() }()
	time.Sleep(100 * time.Millisecond)
	sim.InjectKey(tcell.KeyCtrlC, 0, tcell.ModNone)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestScrollSharedAndFollow(t *testing.T) {
	a, sim := newSim(t, 60, 10) // body height 7
	var rows []convo.Row
	for i := 0; i < 20; i++ {
		rows = append(rows, row(uint64(i), convo.Done, "fr", fmt.Sprintf("ligne%02d", i), fmt.Sprintf("line%02d", i)))
	}
	a.SetRows(rows)
	time.Sleep(100 * time.Millisecond)
	settle(a)
	if out := dump(sim); !strings.Contains(out, "ligne19") {
		t.Fatalf("not following bottom:\n%s", out)
	}
	sim.InjectKey(tcell.KeyPgUp, 0, tcell.ModNone)
	time.Sleep(100 * time.Millisecond)
	settle(a)
	out := dump(sim)
	if strings.Contains(out, "ligne19") {
		t.Fatalf("PgUp did not scroll:\n%s", out)
	}
	// both panes moved together: same index on each side of a screen line
	for _, ln := range strings.Split(out, "\n") {
		if i := strings.Index(ln, "ligne"); i >= 0 {
			n := ln[i+5 : i+7]
			if !strings.Contains(ln, "line"+n) {
				t.Fatalf("panes out of sync: %q", ln)
			}
		}
	}
	// new rows while scrolled up do not yank the view
	a.SetRows(append(rows, row(99, convo.Done, "fr", "dernier", "last")))
	time.Sleep(100 * time.Millisecond)
	settle(a)
	if strings.Contains(dump(sim), "dernier") {
		t.Fatal("scrolled-up view jumped to bottom")
	}
	for i := 0; i < 10; i++ {
		sim.InjectKey(tcell.KeyPgDn, 0, tcell.ModNone)
	}
	time.Sleep(100 * time.Millisecond)
	settle(a)
	if !strings.Contains(dump(sim), "dernier") {
		t.Fatal("PgDn did not return to bottom")
	}
}
