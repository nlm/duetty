package ui

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gdamore/tcell/v2"
	"github.com/nlm/poly/internal/config"
	"github.com/nlm/poly/internal/convo"
	"github.com/rivo/tview"
)

const meterCells = 8

// App is the two-pane terminal UI.
//
// SetRows, SetStatus and SetNotice never block and may be called from any goroutine
// (including from inside the event loop): they store the latest value and
// signal a single coalescing redraw goroutine.
type App struct {
	pair       [2]string
	model      string
	timestamps bool

	app                      *tview.Application
	pages                    *tview.Pages
	hdrL, hdrR, bodyL, bodyR *tview.TextView
	status                   *tview.TextView
	input                    *tview.InputField
	onSubmit                 func(string)
	onCancel, onMute         func()
	helpOpen                 bool

	mu       sync.Mutex // guards the fields below
	rows     []convo.Row
	level    float64
	state    string
	inFlight int
	muted    bool
	notice   string

	redraw   chan struct{}
	done     chan struct{} // closed when Run returns; stops the pump
	pumpDone chan struct{} // closed when the pump goroutine has exited
	once     sync.Once
	stopped  atomic.Bool // Stop was called (possibly before Run)
	live     atomic.Bool // tview screen is up (first draw happened)
	stopCall atomic.Bool // tview Stop invoked (exactly once)
	queued   atomic.Bool // a wake-up event is already in tview's queue

	// Event-loop-only state.
	width, height int
	offset        int  // shared scroll offset, in lines
	following     bool // pinned to the bottom
	nLines        int
}

// New builds the UI for a language pair.
func New(pair [2]string, model string, timestamps bool) *App {
	a := &App{
		pair: pair, model: model, timestamps: timestamps,
		state: "listening", following: true,
		redraw: make(chan struct{}, 1), done: make(chan struct{}), pumpDone: make(chan struct{}),
		app: tview.NewApplication(),
	}
	mk := func() *tview.TextView {
		return tview.NewTextView().SetDynamicColors(true).SetWrap(false).SetScrollable(true)
	}
	a.bodyL, a.bodyR, a.hdrL, a.hdrR = mk(), mk(), mk(), mk()
	a.bodyL.SetDisabled(true)
	a.bodyR.SetDisabled(true)
	a.hdrL.SetText("[::b]" + tview.Escape(config.LanguageName(pair[0])))
	a.hdrR.SetText("[::b]" + tview.Escape(config.LanguageName(pair[1])))
	a.status = tview.NewTextView().SetDynamicColors(true)
	a.input = tview.NewInputField().SetLabel("> ")
	a.input.SetDoneFunc(func(key tcell.Key) {
		if key != tcell.KeyEnter {
			return
		}
		text := strings.TrimSpace(a.input.GetText())
		a.input.SetText("")
		if text != "" && a.onSubmit != nil {
			a.onSubmit(text)
		}
	})

	sep := func() *tview.TextView { return tview.NewTextView().SetText("│") }
	cols := func(l, r tview.Primitive, s tview.Primitive) *tview.Flex {
		f := tview.NewFlex()
		f.AddItem(l, 0, 1, false).AddItem(s, 1, 0, false).AddItem(r, 0, 1, false)
		return f
	}
	bodySep := tview.NewTextView().SetDynamicColors(true)
	bodySep.SetDrawFunc(func(screen tcell.Screen, x, y, w, h int) (int, int, int, int) {
		for i := 0; i < h; i++ {
			screen.SetContent(x, y+i, '│', nil, tcell.StyleDefault.Foreground(tcell.ColorGray))
		}
		return x, y, w, h
	})
	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(cols(a.hdrL, a.hdrR, sep()), 1, 0, false).
		AddItem(cols(a.bodyL, a.bodyR, bodySep), 0, 1, false).
		AddItem(a.status, 1, 0, false).
		AddItem(a.input, 1, 0, true)

	a.pages = tview.NewPages().AddPage("main", root, true, true)
	help := tview.NewTextView().SetText(helpText).SetTextAlign(tview.AlignLeft)
	help.SetBorder(true).SetTitle(" Keys ")
	modal := tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).AddItem(help, 14, 0, false).AddItem(nil, 0, 1, false), 52, 0, false).
		AddItem(nil, 0, 1, false)
	a.pages.AddPage("help", modal, true, false)

	a.app.SetRoot(a.pages, true).SetFocus(a.input).EnableMouse(true)
	a.app.SetBeforeDrawFunc(a.beforeDraw)
	a.app.SetInputCapture(a.handleKey)
	a.app.SetMouseCapture(a.handleMouse)
	a.updateStatusText()
	return a
}

const helpText = `
 Enter      send typed text
 Esc        cancel pending translation
 Ctrl+P     mute / unmute microphone
 PgUp/PgDn  scroll both panes
 Mouse      wheel scrolls
 ?          this help (empty input)
 Ctrl+C     quit

 Press any key to close.`

func isCtrl(ev *tcell.EventKey, k tcell.Key, r rune) bool {
	if ev.Key() == k {
		return true
	}
	return ev.Key() == tcell.KeyRune && ev.Rune() == r && ev.Modifiers() == tcell.ModCtrl
}

// wakeKey is a synthetic key posted by the redraw pump. It never comes from
// a terminal; handleKey consumes it on the event loop, where tview redraws.
const wakeKey = tcell.KeyF64

func (a *App) handleKey(ev *tcell.EventKey) *tcell.EventKey {
	if ev.Key() == wakeKey {
		a.queued.Store(false) // before reading rows: later changes re-signal
		a.refill()
		return nil // tview redraws after a consumed key
	}
	if isCtrl(ev, tcell.KeyCtrlC, 'c') {
		a.Stop()
		return nil
	}
	if a.helpOpen {
		a.pages.HidePage("help")
		a.helpOpen = false
		return nil
	}
	switch {
	case isCtrl(ev, tcell.KeyCtrlP, 'p'):
		if a.onMute != nil {
			a.onMute()
		}
		return nil
	case ev.Key() == tcell.KeyEscape:
		if a.onCancel != nil {
			a.onCancel()
		}
		return nil
	case ev.Key() == tcell.KeyPgUp:
		a.scroll(-a.pageSize())
		return nil
	case ev.Key() == tcell.KeyPgDn:
		a.scroll(a.pageSize())
		return nil
	case ev.Key() == tcell.KeyRune && ev.Rune() == '?' && a.input.GetText() == "":
		a.helpOpen = true
		a.pages.ShowPage("help")
		return nil
	}
	return ev
}

func (a *App) handleMouse(ev *tcell.EventMouse, action tview.MouseAction) (*tcell.EventMouse, tview.MouseAction) {
	switch action {
	case tview.MouseScrollUp:
		a.scroll(-3)
		return nil, 0
	case tview.MouseScrollDown:
		a.scroll(3)
		return nil, 0
	}
	return ev, action
}

func (a *App) pageSize() int {
	if h := a.bodyHeight(); h > 1 {
		return h - 1
	}
	return 1
}

// bodyHeight is the pane height: the screen minus header, status and input.
func (a *App) bodyHeight() int {
	if h := a.height - 3; h > 1 {
		return h
	}
	return 1
}

func (a *App) maxOffset() int {
	if m := a.nLines - a.bodyHeight(); m > 0 {
		return m
	}
	return 0
}

// scroll moves the shared offset; reaching the bottom resumes following.
func (a *App) scroll(delta int) {
	max := a.maxOffset()
	if a.following {
		a.offset = max
	}
	a.offset += delta
	if a.offset < 0 {
		a.offset = 0
	}
	if a.offset >= max {
		a.offset = max
		a.following = true
	} else {
		a.following = false
	}
	a.bodyL.ScrollTo(a.offset, 0)
	a.bodyR.ScrollTo(a.offset, 0)
}

// beforeDraw (event loop) re-lays-out on resize and applies the shared scroll.
func (a *App) beforeDraw(s tcell.Screen) bool {
	a.live.Store(true)
	if a.stopped.Load() {
		go a.stopTview() // Stop raced Run's startup
	}
	w, h := s.Size()
	if w != a.width || h != a.height {
		a.width, a.height = w, h
		a.refill()
	}
	return false
}

// refill (event loop) lays out the latest rows and updates both panes.
func (a *App) refill() {
	if a.width == 0 {
		return
	}
	a.mu.Lock()
	rows := a.rows
	a.mu.Unlock()
	l, r := Layout(rows, a.pair, a.width, a.timestamps)
	a.nLines = len(l)
	a.bodyL.SetText(strings.Join(l, "\n"))
	a.bodyR.SetText(strings.Join(r, "\n"))
	a.bodyL.ScrollTo(a.clampedOffset(), 0)
	a.bodyR.ScrollTo(a.clampedOffset(), 0)
	a.updateStatusText()
}

func (a *App) clampedOffset() int {
	max := a.maxOffset()
	if a.following || a.offset > max {
		a.offset = max
	}
	return a.offset
}

func (a *App) updateStatusText() {
	a.mu.Lock()
	level, state, n, muted, notice := a.level, a.state, a.inFlight, a.muted, a.notice
	a.mu.Unlock()
	if muted {
		state = "muted"
	}
	filled := int(level*meterCells + 0.5)
	if filled < 0 {
		filled = 0
	}
	if filled > meterCells {
		filled = meterCells
	}
	meter := strings.Repeat("█", filled) + strings.Repeat("░", meterCells-filled)
	var sb strings.Builder
	fmt.Fprintf(&sb, "[green]%s[-] %s", meter, tview.Escape(state))
	if n > 0 {
		fmt.Fprintf(&sb, " · working (%d)", n)
	}
	if notice != "" {
		fmt.Fprintf(&sb, " · [yellow]%s[-]", tview.Escape(notice))
	}
	fmt.Fprintf(&sb, " · %s ⇄ %s · %s", tview.Escape(a.pair[0]), tview.Escape(a.pair[1]), tview.Escape(a.model))
	a.status.SetText(sb.String())
}

// signal requests a redraw; it never blocks.
func (a *App) signal() {
	select {
	case a.redraw <- struct{}{}:
	default:
	}
}

// SetRows replaces the displayed rows. Safe from any goroutine; never blocks.
func (a *App) SetRows(rows []convo.Row) {
	a.mu.Lock()
	a.rows = append([]convo.Row(nil), rows...)
	a.mu.Unlock()
	a.signal()
}

// SetStatus updates the status bar. Safe from any goroutine; never blocks.
func (a *App) SetStatus(level float64, state string, inFlight int, muted bool) {
	a.mu.Lock()
	a.level, a.state, a.inFlight, a.muted = level, state, inFlight, muted
	a.mu.Unlock()
	a.signal()
}

// SetNotice shows a short notice (e.g. "smart turn off") in the status bar;
// "" clears it. Safe from any goroutine; never blocks.
func (a *App) SetNotice(notice string) {
	a.mu.Lock()
	a.notice = notice
	a.mu.Unlock()
	a.signal()
}

// OnSubmit, OnCancel and OnMuteToggle register callbacks invoked from the
// event loop (they may call SetRows synchronously). Register before Run.
func (a *App) OnSubmit(f func(string)) { a.onSubmit = f }
func (a *App) OnCancel(f func())       { a.onCancel = f }
func (a *App) OnMuteToggle(f func())   { a.onMute = f }

// Run blocks until Stop or Ctrl+C. It returns nil on a clean quit. A Stop
// that happened before Run makes Run return immediately.
//
// The redraw pump wakes the event loop with QueueEvent of a synthetic key
// rather than QueueUpdate: QueueUpdate waits for the loop to run the update
// and would block forever if the loop has stopped. The pump only ever has
// one wake-up outstanding, so QueueEvent (buffered) does not block either.
func (a *App) Run() error {
	if a.stopped.Load() {
		a.once.Do(func() { close(a.done) })
		close(a.pumpDone)
		return nil
	}
	go func() {
		defer close(a.pumpDone)
		for {
			select {
			case <-a.done:
				return
			case <-a.redraw:
				if a.queued.CompareAndSwap(false, true) {
					a.app.QueueEvent(tcell.NewEventKey(wakeKey, 0, tcell.ModNone))
				}
			}
		}
	}()
	a.signal() // pick up anything set before Run
	err := a.app.Run()
	a.once.Do(func() { close(a.done) })
	return err
}

// Stop ends Run. Safe from any goroutine, any number of times, and before
// Run (in which case Run returns immediately).
func (a *App) Stop() {
	a.stopped.Store(true)
	if a.live.Load() {
		a.stopTview()
	}
	a.signal()
}

// stopTview calls tview's Stop at most once: a second call races with
// tview's unlocked teardown in Run, and before the screen exists it is a
// silent no-op (beforeDraw covers that case).
func (a *App) stopTview() {
	if a.stopCall.CompareAndSwap(false, true) {
		a.app.Stop()
	}
}
