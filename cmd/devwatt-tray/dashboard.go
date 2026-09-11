package main

// The dashboard: one WebView2 window showing everything the tray knows,
// created on first open, hidden on close, destroyed at quit.
//
// Two facts about the libraries shape this file, both read from their
// source (energye/systray v1.0.3, jchv/go-webview2 2026-02-05):
//
// systray's RunWithExternalLoop only registers the tray window (on the
// thread that calls it) and hands back a start function that pumps
// messages on a *different* thread, which can never receive that window's
// messages. The loop that actually serves the tray is therefore ours, on
// the main thread, and start is not called. Quit posts WM_CLOSE to the
// tray window; its WM_DESTROY removes the icon and posts WM_QUIT, which is
// what ends the loop — so the loop must still be running when the close
// is processed, and end() afterwards is a no-op under quitOnce.
//
// go-webview2's Dispatch posts a WM_APP thread message whose work queue
// only its own Run drains, and every JS binding replies through Dispatch.
// So once the window exists, its Run has to be the loop: ours runs until
// the window is created and then hands over. Both loops end on the same
// WM_QUIT. The window's own procedure turns WM_CLOSE into a destroy that
// posts WM_QUIT, which would end the whole tray, so WM_CLOSE is
// intercepted here and hides the window instead.

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unsafe"

	"github.com/energye/systray"
	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"github.com/I-Vishal-Kumar/devwatt/internal/autostart"
	"github.com/I-Vishal-Kumar/devwatt/internal/config"
	"github.com/I-Vishal-Kumar/devwatt/internal/discover"
	"github.com/I-Vishal-Kumar/devwatt/internal/helper"
	"github.com/I-Vishal-Kumar/devwatt/internal/icon"
	"github.com/I-Vishal-Kumar/devwatt/internal/procload"
)

//go:embed dashboard.html
var dashboardHTML string

const (
	wmApp     = 0x8000
	wmClose   = 0x0010
	wmSetIcon = 0x0080
	// wmOpenDashboard is the one thread message our loop understands: open
	// the dashboard, on this thread, outside any window procedure.
	wmOpenDashboard = wmApp + 1

	gwlpWndProc    = ^uintptr(3) // GWLP_WNDPROC is -4; the index travels as a machine word
	iconSmall      = 0
	iconBig        = 1
	imageIcon      = 1
	lrLoadFromFile = 0x0010
)

// msg is MSG from winuser.h.
type msg struct {
	hwnd    windows.HWND
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

// dashboard is the window's state. wv and hwnd are written once, by create
// on the main thread, and read wherever "open" is asked for; they live
// behind the app's ui lock like every other shared piece of state, so that
// nothing here depends on which thread the tray library chooses to call
// back on.
type dashboard struct {
	app  *app
	wv   webview2.WebView
	hwnd windows.HWND
}

// window is the one way to read the pair.
func (d *dashboard) window() (webview2.WebView, windows.HWND) {
	d.app.ui.Lock()
	defer d.app.ui.Unlock()
	return d.wv, d.hwnd
}

// The window's own procedure, kept so everything but WM_CLOSE goes to it.
// Package state rather than a lookup, because there is one window and the
// callback is created once for the process.
var (
	dashOrig uintptr
	dashProc = windows.NewCallback(func(hwnd, m, wp, lp uintptr) uintptr {
		if m == wmClose {
			procShowWindow.Call(hwnd, windows.SW_HIDE)
			return 0
		}
		r, _, _ := procCallWindowProc.Call(dashOrig, hwnd, m, wp, lp)
		return r
	})
)

// loop pumps the main thread's messages until WM_QUIT, or until the
// dashboard has been created — from then on the webview's Run is the loop.
// It reports which.
func (a *app) loop() (quit bool) {
	var m msg
	for {
		r, _, err := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		switch int32(r) {
		case -1:
			fatal(fmt.Errorf("GetMessage: %w", err))
		case 0:
			return true
		}
		if m.hwnd == 0 && m.message == wmOpenDashboard {
			a.dash.open()
			if wv, _ := a.dash.window(); wv != nil {
				return false
			}
			continue
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// openDashboard is what the menu item and a double-click on the icon do.
// Both run inside the tray's window procedure; the work is posted so it
// runs at the top of the loop instead, and through whichever loop is
// current.
func (a *app) openDashboard() {
	if wv, _ := a.dash.window(); wv != nil {
		wv.Dispatch(a.dash.show)
		return
	}
	procPostThreadMessage.Call(uintptr(a.mainThread), wmOpenDashboard, 0, 0)
}

func (d *dashboard) open() {
	if wv, _ := d.window(); wv == nil {
		if err := d.create(); err != nil {
			d.app.setResult("dashboard: " + err.Error())
			return
		}
	}
	d.show()
}

// create makes the window. NewWithOptions shows it at once, which is fine
// because this only runs on a click; it returns nil when the WebView2
// runtime cannot be embedded.
func (d *dashboard) create() error {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return errors.New("APPDATA is not set")
	}
	// The size is in physical pixels now that the process is DPI-aware, so
	// it is scaled from the 96-DPI design size to come up the same physical
	// size on every display.
	dpi := systemDPI()
	wv := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath: filepath.Join(appData, "devwatt", "webview2"),
		WindowOptions: webview2.WindowOptions{
			Title:  "devwatt",
			Width:  1000 * dpi / 96,
			Height: 720 * dpi / 96,
			Center: true,
		},
	})
	if wv == nil {
		return errors.New("WebView2 could not be created; is the WebView2 runtime installed?")
	}
	hwnd := windows.HWND(uintptr(wv.Window()))

	dashOrig, _, _ = procSetWindowLongPtr.Call(uintptr(hwnd), gwlpWndProc, dashProc)
	if dashOrig == 0 {
		return errors.New("SetWindowLongPtr(GWLP_WNDPROC) failed")
	}
	if err := setWindowIcon(hwnd); err != nil {
		d.app.setResult("dashboard icon: " + err.Error())
	}

	for name, fn := range map[string]any{
		"snapshot":         d.app.snapshot,
		"toggle":           d.app.toggleByName,
		"adopt":            d.app.adoptByName,
		"setPolicy":        d.app.setPolicyTo,
		"setNotifications": func(on bool) error { go d.app.setNotifications(on); return nil },
		"clearNotices":     d.app.clearNotices,
		"setTheme":         d.app.setTheme,
		"setBaseline":      func() error { go d.app.setBaseline(); return nil },
		"setThresholds":    d.app.setThresholds,
		"setAutostart":     d.app.setAutostart,
		"quit":             func() error { systray.Quit(); return nil },
	} {
		if err := wv.Bind(name, fn); err != nil {
			return fmt.Errorf("bind %s: %w", name, err)
		}
	}
	wv.SetHtml(dashboardHTML)

	d.app.ui.Lock()
	d.wv, d.hwnd = wv, hwnd
	d.app.ui.Unlock()
	return nil
}

// show brings the window to the front, restoring it if it was minimised.
func (d *dashboard) show() {
	_, hwnd := d.window()
	cmd := uintptr(windows.SW_SHOW)
	if r, _, _ := procIsIconic.Call(uintptr(hwnd)); r != 0 {
		cmd = windows.SW_RESTORE
	}
	procShowWindow.Call(uintptr(hwnd), cmd)
	procSetForegroundWindow.Call(uintptr(hwnd))
}

// destroy tears the window down at quit. The loop has ended by then, so the
// WM_QUIT the webview's procedure posts from WM_DESTROY goes nowhere.
func (d *dashboard) destroy() {
	wv, hwnd := d.window()
	if wv == nil {
		return
	}
	procDestroyWindow.Call(uintptr(hwnd))
}

// setWindowIcon gives the window the tray's icon. LoadImage reads icons
// from files or resources, not memory, so the embedded bytes go through a
// temp file, as systray does for the tray icon itself.
func setWindowIcon(hwnd windows.HWND) error {
	path := filepath.Join(os.TempDir(), "devwatt.ico")
	if err := os.WriteFile(path, icon.Bytes, 0o644); err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	for _, size := range []struct{ which, px uintptr }{{iconSmall, 16}, {iconBig, 32}} {
		h, _, err := procLoadImage.Call(0, uintptr(unsafe.Pointer(p)), imageIcon, size.px, size.px, lrLoadFromFile)
		if h == 0 {
			return fmt.Errorf("LoadImage(%s): %w", path, err)
		}
		procSendMessage.Call(uintptr(hwnd), wmSetIcon, size.which, h)
	}
	return nil
}

// view is what the page shows, all of it, in one snapshot.
type view struct {
	Now            string          `json:"now"`
	OnBattery      bool            `json:"onBattery"`
	Percent        int             `json:"percent"`
	RateMW         int             `json:"rateMW"`   // signed: charging positive, discharging negative
	Charging       bool            `json:"charging"` // on AC and the rate is positive
	Status         string          `json:"status"`
	Result         string          `json:"result"`
	AvgMW          int             `json:"avgMW"`
	SwingMW        int             `json:"swingMW"`
	Steady         bool            `json:"steady"`
	Full           bool            `json:"full"`
	HoursLeft      float64         `json:"hoursLeft"` // the driver's estimate, as Windows shows it
	HoursKnown     bool            `json:"hoursKnown"`
	CPU            float64         `json:"cpu"`
	BaselineMW     int             `json:"baselineMW"`
	History        []point         `json:"history"`
	Managed        []managedView   `json:"managed"`
	Suggested      []suggestedView `json:"suggested"`
	Processes      []procload.Proc `json:"processes"`
	Notices        []notice        `json:"notices"`
	Settings       settingsView    `json:"settings"`
	Autostart      bool            `json:"autostart"`
	AutostartError string          `json:"autostartError"`
}

// point is one discharge reading for the chart.
type point struct {
	T  int64 `json:"t"` // Unix milliseconds
	MW int   `json:"mw"`
}

type managedView struct {
	Name    string    `json:"name"`
	Display string    `json:"display"`
	Note    string    `json:"note"`
	Running bool      `json:"running"`
	Busy    bool      `json:"busy"`
	Cost    *costView `json:"cost"`
}

type costView struct {
	Action    string  `json:"action"`
	DeltaMW   int     `json:"deltaMW"`
	BeforeMW  int     `json:"beforeMW"`
	AfterMW   int     `json:"afterMW"`
	CPUBefore float64 `json:"cpuBefore"`
	CPUAfter  float64 `json:"cpuAfter"`
	Void      string  `json:"void"`
	When      string  `json:"when"`
}

type suggestedView struct {
	Name    string `json:"name"`
	Display string `json:"display"`
	Path    string `json:"path"`
	Adopted bool   `json:"adopted"`
}

// notice is one alert as the page lists it; Shown says whether it was also
// a toast, or logged only because the switch was off or the day's cap spent.
type notice struct {
	When  string `json:"when"`
	Title string `json:"title"`
	Text  string `json:"text"`
	Shown bool   `json:"shown"`
}

type settingsView struct {
	StopOnBattery     bool   `json:"stopOnBattery"`
	Notifications     bool   `json:"notifications"`
	MaxToastsPerDay   int    `json:"maxToastsPerDay"`
	HighDrawPercent   int    `json:"highDrawPercent"`
	HogCPUPercent     int    `json:"hogCPUPercent"`
	HogSustainSeconds int    `json:"hogSustainSeconds"`
	BaselineMW        int    `json:"baselineMW"`
	Theme             string `json:"theme"`
}

// snapshot is the page's one read. It holds ui only briefly and never
// waits on mu, so the page keeps updating through a cost run. The autostart
// state is a registry read; a failure there is reported in the view rather
// than failing the whole page.
func (a *app) snapshot() (view, error) {
	installed, autoErr := autostart.Installed()

	a.ui.Lock()
	defer a.ui.Unlock()

	// Every list is a non-nil slice: the page calls map and filter on them,
	// and JSON's null for an empty Go slice would break it.
	now := time.Now()
	v := view{
		Now:        now.Format("15:04:05"),
		OnBattery:  a.latestOnBattery,
		Percent:    a.percent,
		RateMW:     int(a.rateMW),
		Charging:   !a.latestOnBattery && a.rateMW > 0,
		Status:     a.statusText,
		Result:     a.resultText,
		AvgMW:      a.latest.AvgMW,
		SwingMW:    a.latest.SwingMW,
		Steady:     a.latest.Trustworthy(),
		Full:       a.latestFull,
		HoursLeft:  a.hoursLeft,
		HoursKnown: a.hoursKnown,
		CPU:        a.cpu,
		BaselineMW: a.cfg.BaselineMW,
		History:    append(make([]point, 0, len(a.history)), a.history...),
		Processes:  append(make([]procload.Proc, 0, len(a.top)), a.top...),
		Notices:    append(make([]notice, 0, len(a.notices)), a.notices...),
		Managed:    make([]managedView, 0, len(a.managed)),
		Suggested:  make([]suggestedView, 0, len(a.suggested)),
		Settings: settingsView{
			StopOnBattery:     a.cfg.StopOnBattery,
			Notifications:     a.cfg.Notifications,
			MaxToastsPerDay:   a.cfg.MaxToastsPerDay,
			HighDrawPercent:   a.cfg.HighDrawPercent,
			HogCPUPercent:     a.cfg.HogCPUPercent,
			HogSustainSeconds: a.cfg.HogSustainSeconds,
			BaselineMW:        a.cfg.BaselineMW,
			Theme:             a.cfg.Theme,
		},
		Autostart: installed,
	}
	if autoErr != nil {
		v.AutostartError = autoErr.Error()
	}
	for _, c := range a.managed {
		m := managedView{
			Name:    c.Name,
			Display: displayName(c),
			Note:    c.BinaryPath,
			Running: a.running[c.Name],
			Busy:    a.busy[c.Name],
		}
		if c.InCatalog {
			m.Note = c.Entry.Note
		}
		if cost, ok := a.cfg.Costs[c.Name]; ok {
			m.Cost = &costView{
				Action:    cost.Action,
				DeltaMW:   cost.DeltaMW,
				BeforeMW:  cost.BeforeMW,
				AfterMW:   cost.AfterMW,
				CPUBefore: cost.CPUBefore,
				CPUAfter:  cost.CPUAfter,
				Void:      cost.Void,
				When:      when(cost.When, now),
			}
		}
		v.Managed = append(v.Managed, m)
	}
	for _, c := range a.suggested {
		v.Suggested = append(v.Suggested, suggestedView{
			Name:    c.Name,
			Display: c.Display,
			Path:    c.BinaryPath,
			Adopted: a.cfg.IsAdopted(c.Name),
		})
	}
	return v, nil
}

// when shows a time the way a person reads it next to "now": the clock if
// it was today, the date as well if not.
func when(t, now time.Time) string {
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// toggleByName is the page's Stop/Start button: the same path as a tray
// click, started and returned from, so the page sees progress in snapshot.
func (a *app) toggleByName(name string) error {
	a.ui.Lock()
	item, ok := a.items[name]
	var c discover.Candidate
	for _, m := range a.managed {
		if m.Name == name {
			c = m
		}
	}
	a.ui.Unlock()
	if !ok {
		return fmt.Errorf("%s is not a managed service", name)
	}
	go a.toggle(item, c)
	return nil
}

// adoptByName is the page's Adopt/Un-adopt button.
func (a *app) adoptByName(name string, on bool) error {
	a.ui.Lock()
	known := false
	for _, c := range a.suggested {
		if c.Name == name {
			known = true
		}
	}
	a.ui.Unlock()
	if !known {
		return fmt.Errorf("%s is not a suggested service", name)
	}
	go a.adopt(name, on)
	return nil
}

// clearNotices is the page's "Clear all": it empties the log and nothing
// else. The day's toast count stays — the cap is about interruptions, and
// clearing the record of them does not un-interrupt anyone.
func (a *app) clearNotices() error {
	a.ui.Lock()
	defer a.ui.Unlock()
	a.notices = nil
	return nil
}

// setTheme is the page's appearance control. Validation matches
// config.Load's, so the file can never hold what Load would refuse.
func (a *app) setTheme(name string) error {
	if !slices.Contains(config.Themes, name) {
		return fmt.Errorf("theme must be one of %s", strings.Join(config.Themes, ", "))
	}
	var prev string
	return a.editConfig(
		func(c *config.Config) bool { prev = c.Theme; c.Theme = name; return prev != name },
		func(c *config.Config) { c.Theme = prev })
}

// setPolicyTo is the page's policy checkbox.
func (a *app) setPolicyTo(on bool) error {
	go a.setPolicy(on)
	return nil
}

// setThresholds is the page's Save. Validation is the same as config.Load's,
// so the file can never hold what Load would refuse.
func (a *app) setThresholds(highDrawPercent, hogCPUPercent, hogSustainSeconds, baselineMW, maxToastsPerDay int) error {
	for _, t := range []struct {
		field string
		value int
	}{
		{"high_draw_percent", highDrawPercent},
		{"hog_cpu_percent", hogCPUPercent},
		{"hog_sustain_seconds", hogSustainSeconds},
		{"max_toasts_per_day", maxToastsPerDay},
	} {
		if t.value <= 0 {
			return fmt.Errorf("%s must be greater than 0", t.field)
		}
	}
	if baselineMW < 0 {
		return errors.New("baseline_mw must not be negative")
	}
	var prev config.Config
	return a.editConfig(
		func(c *config.Config) bool {
			prev = *c
			c.HighDrawPercent, c.HogCPUPercent, c.HogSustainSeconds, c.BaselineMW, c.MaxToastsPerDay =
				highDrawPercent, hogCPUPercent, hogSustainSeconds, baselineMW, maxToastsPerDay
			return true
		},
		func(c *config.Config) {
			c.HighDrawPercent, c.HogCPUPercent, c.HogSustainSeconds, c.BaselineMW, c.MaxToastsPerDay =
				prev.HighDrawPercent, prev.HogCPUPercent, prev.HogSustainSeconds, prev.BaselineMW, prev.MaxToastsPerDay
		})
}

// setAutostart is the page's "Start at logon" checkbox. It registers this
// very executable — the one the user is running — as the tray's own task,
// which runs at the user's level and so needs no elevation to create; the
// helper's task is install-only. It works off the UI thread because
// schtasks takes a moment; the outcome lands in the result line and the
// checkbox follows the registry on the next snapshot.
func (a *app) setAutostart(on bool) error {
	go func() {
		// The task belongs to the elevated install; only the helper can
		// change it.
		err := helper.Client{}.SetAutostart(on)
		if err != nil {
			a.setResult("autostart: " + err.Error())
			return
		}
		if on {
			a.setResult("Start at logon: on")
		} else {
			a.setResult("Start at logon: off")
		}
	}()
	return nil
}

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procGetMessage          = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessage     = user32.NewProc("DispatchMessageW")
	procPostThreadMessage   = user32.NewProc("PostThreadMessageW")
	procSetWindowLongPtr    = user32.NewProc("SetWindowLongPtrW")
	procCallWindowProc      = user32.NewProc("CallWindowProcW")
	procShowWindow          = user32.NewProc("ShowWindow")
	procIsIconic            = user32.NewProc("IsIconic")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procLoadImage           = user32.NewProc("LoadImageW")
	procSendMessage         = user32.NewProc("SendMessageW")

	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDpiForSystem               = user32.NewProc("GetDpiForSystem")
)
