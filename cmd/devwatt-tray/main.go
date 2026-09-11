// Command devwatt-tray is the tray application: a status line that reads
// like the measure command, a checkbox per managed service that stops or
// starts it and prices the change, one policy — stop the managed set when
// the charger comes out, and put back exactly what it stopped when the
// charger returns — notifications that name what is draining the battery
// while it happens, and a dashboard window that shows all of it at once.
//
// It runs as the user, unelevated, and if started elevated it has its logon
// task start an unelevated copy instead: an elevated window cannot take
// input from the user's other programs (UIPI), which is how Print Screen
// stopped working with the dashboard focused. The one thing that needs
// administrator rights — stopping and starting a service — is done by the
// same executable running as `--helper`, elevated by its scheduled task
// and reached over a named pipe; see the helper package. Reading stays
// cheap: the status line comes from the same battery and CPU reads the CLI
// uses, taken on the ACPI driver's own cadence, so an idle tray costs the
// machine nothing it can measure.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/energye/systray"
	"golang.org/x/sys/windows"

	"github.com/I-Vishal-Kumar/devwatt/internal/alert"
	"github.com/I-Vishal-Kumar/devwatt/internal/battery"
	"github.com/I-Vishal-Kumar/devwatt/internal/config"
	"github.com/I-Vishal-Kumar/devwatt/internal/control"
	"github.com/I-Vishal-Kumar/devwatt/internal/cost"
	"github.com/I-Vishal-Kumar/devwatt/internal/cpuload"
	"github.com/I-Vishal-Kumar/devwatt/internal/discover"
	"github.com/I-Vishal-Kumar/devwatt/internal/elevate"
	"github.com/I-Vishal-Kumar/devwatt/internal/helper"
	"github.com/I-Vishal-Kumar/devwatt/internal/icon"
	"github.com/I-Vishal-Kumar/devwatt/internal/notify"
	"github.com/I-Vishal-Kumar/devwatt/internal/procload"
)

// pollInterval is the ACPI driver's own refresh cadence (see battery.go):
// reading faster returns the same value again, and slower leaves readings
// on the table that the window could have had.
const pollInterval = 2500 * time.Millisecond

// topProcs is how many programs the rules get to look at each tick; the
// notices only ever name the first two.
const topProcs = 8

// noResult is the result line before anything has been priced. The item is
// never hidden: systray tracks menu positions by item id across ResetMenu
// without clearing them, so an item shown after a rebuild lands at the
// bottom of the menu instead of where it was created.
const noResult = "No result yet - tick a managed service to price it"

// The page keeps this much: five minutes of readings at the poll cadence,
// and enough notices to see an afternoon.
const (
	historyLen = 120
	noticesLen = 50
)

func init() {
	// Every window this process makes — the tray's and the dashboard's —
	// belongs to the thread that runs main, and so does the loop that
	// serves them. Neither may wander to another thread.
	runtime.LockOSThread()
}

func main() {
	// First, before anything makes a window: a process's DPI awareness is
	// fixed the moment its first window exists, and systray's Register
	// creates one. Without this Windows renders the dashboard at 96 DPI and
	// stretches the bitmap to the display's scale.
	if err := setDPIAware(); err != nil {
		fatal(err)
	}
	mode, err := parseArgs(os.Args[1:])
	if err != nil {
		fatal(err)
	}

	// The helper is this executable in its other role: elevated by its
	// task, windowless, serving the pipe until it is stopped. A second one
	// finds the pipe taken and leaves.
	if mode == modeHelper {
		if err := helper.Serve(); !errors.Is(err, helper.ErrAlreadyRunning) {
			fatal(err)
		}
		return
	}

	// The tray runs as the user. Started elevated — an installer's finish
	// page, "Run as administrator" — it has its own logon task start an
	// unelevated copy, hands it any request, and leaves.
	if elevate.Elevated() {
		if err := startAsUser(mode); err != nil {
			fatal(err)
		}
		return
	}

	// One tray per user. A second launch hands its request to the first and
	// leaves quietly: a second icon is never what was wanted.
	if hwnd, running, err := otherInstance(); err != nil {
		fatal(err)
	} else if running {
		if mode == modeDashboard {
			if err := handOff(hwnd); err != nil {
				fatal(err)
			}
		}
		return
	}

	a := &app{
		mainThread:    windows.GetCurrentThreadId(),
		wantDashboard: mode == modeDashboard,
		statusText:    "Starting...",
		resultText:    noResult,
	}
	a.dash = &dashboard{app: a}

	// RunWithExternalLoop registers the tray window on this thread. The
	// start function it returns pumps a different thread's queue and would
	// never see this window's messages, so it is not called; see dashboard.go.
	_, end := systray.RunWithExternalLoop(a.onReady, nil)
	if err := a.acceptHandOff(); err != nil {
		fatal(err)
	}
	if !a.loop() {
		wv, _ := a.dash.window()
		wv.Run()
	}
	end()
	a.dash.destroy()
}

// dpiAwarenessPerMonitorV2 is DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, a
// pseudo-handle whose value is -4.
const dpiAwarenessPerMonitorV2 = ^uintptr(3)

// setDPIAware asks for per-monitor DPI awareness. It fails only on Windows
// before 1809 or once a window already exists, neither of which this
// program accepts.
func setDPIAware() error {
	if r, _, err := procSetProcessDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2); r == 0 {
		return fmt.Errorf("SetProcessDpiAwarenessContext: %w", err)
	}
	return nil
}

// systemDPI is the display's DPI, 96 meaning 100%.
func systemDPI() uint {
	r, _, _ := procGetDpiForSystem.Call()
	return uint(r)
}

// fatal shows the error and exits. A tray application has no terminal, and
// an error nobody sees is the same as silent failure.
func fatal(err error) {
	// The only way the conversions fail is an interior NUL, which no error
	// string here can carry; a nil pointer would still show the box.
	text, _ := windows.UTF16PtrFromString(err.Error())
	caption, _ := windows.UTF16PtrFromString("devwatt")
	windows.MessageBox(0, text, caption, windows.MB_OK|windows.MB_ICONERROR)
	os.Exit(1)
}

func (a *app) onReady() {
	systray.SetIcon(icon.Bytes)
	systray.SetTooltip("devwatt")
	systray.SetOnDClick(func(systray.IMenu) { a.openDashboard() })

	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	a.cfg = cfg
	if err := a.buildMenu(); err != nil {
		fatal(err)
	}
	go a.poll()
	if a.wantDashboard {
		a.openDashboard()
	}
}

// app is the tray's state.
//
// Two locks, for two reasons. mu serialises everything that acts — a
// toggle, an adoption, a refresh, the on-battery policy, setting the
// baseline — so two clicks, or a click and the policy, never run control
// concurrently; a toggle holds it for the length of a cost run. ui guards
// the state itself: the config, the managed set and its checkboxes, the two
// text lines, and the latest reading. Every read or write of those takes
// ui, and holds it briefly, so the poll loop can keep the status line
// moving and the rules fed while mu is held for half a minute.
type app struct {
	mu sync.Mutex

	// mainThread and dash belong to the main thread only; wantDashboard is
	// set once from the command line.
	mainThread    uint32
	dash          *dashboard
	wantDashboard bool

	ui  sync.Mutex
	cfg config.Config

	// managed, suggested, items and running are what the last rebuild
	// produced, kept so the policy, the rules and the page know what is
	// managed and in what state without a rebuild of their own. running is
	// updated wherever a box is checked or unchecked, so it is the state the
	// menu shows; busy marks a service with a toggle in flight.
	managed    []discover.Candidate
	suggested  []discover.Candidate
	items      map[string]*systray.MenuItem
	running    map[string]bool
	busy       map[string]bool
	policyItem *systray.MenuItem
	notifyItem *systray.MenuItem

	status     *systray.MenuItem
	result     *systray.MenuItem
	statusText string
	resultText string

	// The poll loop's most recent view of the machine, published for the
	// baseline menu item and the page, neither of which can read the window
	// or the process snapshots themselves.
	latest          battery.Measurement
	latestFull      bool
	latestOnBattery bool
	percent         int
	rateMW          int32 // signed, as the controller reports it: charging is positive
	cpu             float64
	top             []procload.Proc

	// history is the last historyLen discharge readings, appended while on
	// battery and kept across a plug-in, so the chart shows the session
	// even after the charger goes back in. notices is every alert raised,
	// newest first, shown or not; toastDay and toastCount are the day's
	// spend against the cap.
	history    []point
	notices    []notice
	toastDay   string
	toastCount int

	// stoppedByPolicy is what the policy stopped and therefore what it
	// starts again — the candidate, not just the name, so the result line
	// can call it by its display name even if a rebuild has since dropped
	// it from managed. Under mu.
	stoppedByPolicy []discover.Candidate

	// window, onAC and seenAC are touched only by the poll goroutine.
	window battery.Window
	onAC   bool
	seenAC bool
}

// buildMenu replaces the whole menu from a fresh discovery. systray cannot
// remove or reorder a single item, so any change to the managed set is a
// rebuild, and the status and result lines are re-created with their text.
// Discovery runs first, so a failure leaves the old menu standing.
func (a *app) buildMenu() error {
	cands, err := discover.List()
	if err != nil {
		return err
	}

	a.ui.Lock()
	defer a.ui.Unlock()

	var managed, suggested []discover.Candidate
	for _, c := range cands {
		if c.InCatalog || a.cfg.IsAdopted(c.Name) {
			managed = append(managed, c)
		}
		if !c.InCatalog {
			suggested = append(suggested, c)
		}
	}
	sort.Slice(managed, func(i, j int) bool { return managed[i].Name < managed[j].Name })
	sort.Slice(suggested, func(i, j int) bool { return suggested[i].Name < suggested[j].Name })

	systray.ResetMenu()
	// Tooltips are "" throughout: systray never hands per-item tooltips to
	// Win32 on Windows, and passing text a library drops is misleading.
	systray.AddMenuItem("Open dashboard", "").Click(a.openDashboard)
	a.status = systray.AddMenuItem(a.statusText, "")
	a.status.Disable()
	a.result = systray.AddMenuItem(a.resultText, "")
	a.result.Disable()
	systray.AddSeparator()

	systray.AddMenuItem("Managed", "").Disable()
	a.managed = managed
	a.suggested = suggested
	a.items = make(map[string]*systray.MenuItem, len(managed))
	a.running = make(map[string]bool, len(managed))
	a.busy = make(map[string]bool, len(managed))
	for _, c := range managed {
		item := systray.AddMenuItemCheckbox(fmt.Sprintf("%s (%s)", displayName(c), c.Name), "", c.Running)
		a.items[c.Name] = item
		a.running[c.Name] = c.Running
		// Click handlers run on the message loop; a cost run takes half a
		// minute, so the work goes to a goroutine and mu keeps it single-file.
		item.Click(func() { go a.toggle(item, c) })
	}

	sugg := systray.AddMenuItem("Suggested", "")
	if len(suggested) == 0 {
		sugg.Disable()
	}
	for _, c := range suggested {
		item := sugg.AddSubMenuItemCheckbox(c.Display, "", a.cfg.IsAdopted(c.Name))
		name := c.Name
		item.Click(func() { go a.adopt(name, !a.isAdopted(name)) })
	}

	systray.AddSeparator()
	a.policyItem = systray.AddMenuItemCheckbox("Stop managed services on battery", "", a.cfg.StopOnBattery)
	a.policyItem.Click(func() { go a.setPolicy(!a.policyOn()) })
	a.notifyItem = systray.AddMenuItemCheckbox("Notifications", "", a.cfg.Notifications)
	a.notifyItem.Click(func() { go a.setNotifications(!a.notificationsOn()) })
	systray.AddMenuItem("Set current draw as baseline", "").Click(func() { go a.setBaseline() })

	systray.AddSeparator()
	systray.AddMenuItem("Refresh", "").Click(func() { go a.refresh() })
	systray.AddMenuItem("Quit", "").Click(systray.Quit)
	return nil
}

// displayName is what the tray calls a managed service: the catalog's name
// for a catalog match, the service's own display name for an adopted one.
func displayName(c discover.Candidate) string {
	if c.InCatalog {
		return c.Entry.Display
	}
	return c.Display
}

// toggle stops or starts one managed service and prices the change, then
// asks the service what state it is really in rather than assuming.
func (a *app) toggle(item *systray.MenuItem, c discover.Candidate) {
	a.mu.Lock()
	defer a.mu.Unlock()

	item.Disable()
	a.setBusy(c.Name, true)
	defer func() {
		a.setBusy(c.Name, false)
		item.Enable()
	}()

	// Decide from the service, not the checkbox. The user may have changed
	// it in services.msc since the menu was built; stopping a stopped
	// service is a no-op that would then be priced as if it happened.
	running, err := control.Running(c.Name)
	if err != nil {
		a.setResult(fmt.Sprintf("%s: %v", c.Name, err))
		return
	}
	action := cost.Start
	if running {
		action = cost.Stop
	}
	line := a.act(c.Name, action)

	running, err = control.Running(c.Name)
	if err != nil {
		line = fmt.Sprintf("%s; state unknown: %v", line, err)
	} else {
		a.setRunning(c.Name, running)
	}
	a.setResult(line)
	a.notify(displayName(c), line)
}

// act performs the toggle and returns the result line: measured when the
// machine is on battery, merely done when it is not. A measurement is
// remembered in the config, so a later "running on battery" notice can say
// what the service was last found to cost.
func (a *app) act(name string, action cost.Action) string {
	s, err := battery.Read()
	if err != nil {
		return fmt.Sprintf("%s: %v", name, err)
	}
	ctl := helper.Client{}
	if s.AcOnline {
		if action == cost.Stop {
			err = ctl.Stop(name)
		} else {
			err = ctl.Start(name)
		}
		if err != nil {
			return fmt.Sprintf("%s: %v", name, err)
		}
		return fmt.Sprintf("%s %s (on AC - not measured)", name, action)
	}
	r, err := cost.Run(context.Background(), name, action, trayObserver{a: a, name: name, action: action}, ctl)
	if err != nil {
		return fmt.Sprintf("%s: %v", name, err)
	}

	reason, _ := r.Void()
	rec := config.Cost{
		Action:    action.String(),
		DeltaMW:   r.DeltaMW(),
		BeforeMW:  r.Before.AvgMW,
		AfterMW:   r.After.AvgMW,
		CPUBefore: r.CPUBefore,
		CPUAfter:  r.CPUAfter,
		Void:      reason,
		When:      time.Now(),
	}
	var prev config.Cost
	var had bool
	err = a.editConfig(
		func(c *config.Config) bool { prev, had = c.Costs[name]; c.Costs[name] = rec; return true },
		func(c *config.Config) {
			if had {
				c.Costs[name] = prev
			} else {
				delete(c.Costs, name)
			}
		})
	if err != nil {
		return fmt.Sprintf("%s; %v", r, err)
	}
	return r.String()
}

// trayObserver narrates a cost run in the result line: the baseline as soon
// as it lands, the transition as soon as the service reports it.
type trayObserver struct {
	a      *app
	name   string
	action cost.Action
}

func (o trayObserver) Baseline(m battery.Measurement, cpu float64) {
	o.a.setResult(fmt.Sprintf("%s: baseline %s, %s...", o.name, battery.Watts(m.AvgMW), o.action.Progressive()))
}

func (o trayObserver) Transitioned(took time.Duration) {
	o.a.setResult(fmt.Sprintf("%s %s in %.1fs, settling...", o.name, o.action, took.Seconds()))
}

// adopt puts one suggested service in or out of the managed set, saves, and
// rebuilds so it appears or disappears under Managed.
func (a *app) adopt(name string, on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var was bool
	err := a.editConfig(
		func(c *config.Config) bool { was = c.IsAdopted(name); c.SetAdopted(name, on); return was != on },
		func(c *config.Config) { c.SetAdopted(name, was) })
	if err != nil {
		a.setResult(err.Error())
		return
	}
	if err := a.buildMenu(); err != nil {
		a.setResult(err.Error())
	}
}

func (a *app) isAdopted(name string) bool {
	a.ui.Lock()
	defer a.ui.Unlock()
	return a.cfg.IsAdopted(name)
}

func (a *app) refresh() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.buildMenu(); err != nil {
		a.setResult(err.Error())
	}
}

// setPolicy sets the on-battery policy, saves it, and shows it on the menu.
func (a *app) setPolicy(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var was bool
	err := a.editConfig(
		func(c *config.Config) bool { was = c.StopOnBattery; c.StopOnBattery = on; return was != on },
		func(c *config.Config) { c.StopOnBattery = was })
	if err != nil {
		a.setResult(err.Error())
		return
	}
	a.ui.Lock()
	defer a.ui.Unlock()
	if on {
		a.policyItem.Check()
	} else {
		a.policyItem.Uncheck()
	}
}

func (a *app) policyOn() bool {
	a.ui.Lock()
	defer a.ui.Unlock()
	return a.cfg.StopOnBattery
}

// setNotifications sets the toast switch, saves it, and shows it on the menu.
func (a *app) setNotifications(on bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var was bool
	err := a.editConfig(
		func(c *config.Config) bool { was = c.Notifications; c.Notifications = on; return was != on },
		func(c *config.Config) { c.Notifications = was })
	if err != nil {
		a.setResult(err.Error())
		return
	}
	a.ui.Lock()
	defer a.ui.Unlock()
	if on {
		a.notifyItem.Check()
	} else {
		a.notifyItem.Uncheck()
	}
}

func (a *app) notificationsOn() bool {
	a.ui.Lock()
	defer a.ui.Unlock()
	return a.cfg.Notifications
}

// setBaseline records the current sustained draw as the baseline, if there
// is a current sustained draw: on battery, a full window, a steady one.
func (a *app) setBaseline() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.ui.Lock()
	m, full, onBattery := a.latest, a.latestFull, a.latestOnBattery
	a.ui.Unlock()

	switch {
	case !onBattery:
		a.setResult("Baseline not set: not on battery")
		return
	case !full:
		a.setResult("Baseline not set: still measuring")
		return
	case !m.Trustworthy():
		a.setResult(fmt.Sprintf("Baseline not set: unstable (swing %s)", battery.Watts(m.SwingMW)))
		return
	}

	var prev int
	err := a.editConfig(
		func(c *config.Config) bool { prev = c.BaselineMW; c.BaselineMW = m.AvgMW; return true },
		func(c *config.Config) { c.BaselineMW = prev })
	if err != nil {
		a.setResult(err.Error())
		return
	}
	a.setResult("Baseline set: " + battery.Watts(m.AvgMW))
}

// editConfig applies change under ui and saves, unless change reports it
// changed nothing. A save that fails is undone with undo, so memory never
// shows a state the file does not.
func (a *app) editConfig(change func(*config.Config) bool, undo func(*config.Config)) error {
	a.ui.Lock()
	defer a.ui.Unlock()

	if !change(&a.cfg) {
		return nil
	}
	err := config.Save(a.cfg)
	if err != nil {
		undo(&a.cfg)
	}
	return err
}

// policyStop is the on-battery policy: stop every managed service that is
// running and remember which, so policyStart can undo exactly that and no
// more. It is plain control, not cost.Run — the AC transition is itself a
// confounder, so a price taken here would always be void.
//
// The policy flag and the power state are both checked here, under mu,
// rather than by the poll loop that spawned this: a toggle may have held mu
// long enough for the user to tick the box or plug the charger back in, and
// what counts is the state when the policy can actually act.
func (a *app) policyStop() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.ui.Lock()
	on := a.cfg.StopOnBattery
	managed := a.managed
	a.ui.Unlock()
	if !on {
		return
	}
	s, err := battery.Read()
	if err != nil {
		a.setResult(err.Error())
		return
	}
	if s.AcOnline {
		return
	}

	var stopped, errs []string
	for _, c := range managed {
		running, err := control.Running(c.Name)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", c.Name, err))
			continue
		}
		if !running {
			continue
		}
		if err := (helper.Client{}).Stop(c.Name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", c.Name, err))
			continue
		}
		a.stoppedByPolicy = append(a.stoppedByPolicy, c)
		a.setRunning(c.Name, false)
		stopped = append(stopped, displayName(c))
	}

	line := "On battery: nothing to stop"
	if len(stopped) > 0 {
		line = "On battery: stopped " + strings.Join(stopped, ", ")
	}
	line = withErrors(line, errs)
	a.setResult(line)
	a.notify("On battery", line)
}

// policyStart undoes policyStop when the charger returns: every service the
// policy stopped is started again, whether or not the box is still ticked,
// because what matters is what devwatt itself changed. If the charger is
// already out again by the time mu is free, the set is kept for the next
// time it comes back.
func (a *app) policyStart() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.stoppedByPolicy) == 0 {
		return
	}
	s, err := battery.Read()
	if err != nil {
		a.setResult(err.Error())
		return
	}
	if !s.AcOnline {
		return
	}

	var started, errs []string
	for _, c := range a.stoppedByPolicy {
		if err := (helper.Client{}).Start(c.Name); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", c.Name, err))
			continue
		}
		a.setRunning(c.Name, true)
		started = append(started, displayName(c))
	}
	a.stoppedByPolicy = nil

	line := "On AC: nothing started"
	if len(started) > 0 {
		line = "On AC: started " + strings.Join(started, ", ")
	}
	line = withErrors(line, errs)
	a.setResult(line)
	a.notify("On AC", line)
}

// withErrors appends each error to a result line, so a policy run that
// half-worked says exactly which half.
func withErrors(line string, errs []string) string {
	for _, e := range errs {
		line += "; " + e
	}
	return line
}

// poll keeps the status line current, notices AC transitions for the
// policy, and feeds the rules every tick with the reading, the CPU load,
// the top programs and the managed set.
func (a *app) poll() {
	prev, err := cpuload.Take()
	if err != nil {
		a.setStatus(err.Error())
		return
	}
	prevProc, err := procload.Take()
	if err != nil {
		a.setStatus(err.Error())
		return
	}
	var rules alert.Rules

	for ; ; time.Sleep(pollInterval) {
		s, err := battery.Read()
		if err != nil {
			a.setStatus(err.Error())
			continue
		}

		// The first tick only records; a transition is a change from a
		// state that was seen, not from an unknown one. The policy runs on
		// its own goroutine because it waits on mu, which a toggle may hold.
		if a.seenAC && a.onAC && !s.AcOnline {
			go a.policyStop()
		}
		if a.seenAC && !a.onAC && s.AcOnline {
			go a.policyStart()
		}
		a.onAC, a.seenAC = s.AcOnline, true

		cur, err := cpuload.Take()
		if err != nil {
			a.setStatus(err.Error())
			continue
		}
		cpu := cpuload.Percent(prev, cur)
		prev = cur

		procs, err := procload.Take()
		if err != nil {
			a.setResult(err.Error())
			continue
		}
		top := procload.Top(prevProc, procs, topProcs)
		prevProc = procs

		// Only a discharge reading goes into the window: on the tick the
		// charger goes in the controller reports 0 before AcOnline flips,
		// and that tick shows as still measuring rather than as a 0 W dip.
		var m battery.Measurement
		full := false
		if s.AcOnline {
			a.window.Reset()
			a.setStatus(fmt.Sprintf("On AC (%d%%) - unplug to measure", s.Percent()))
		} else {
			if s.Discharging() {
				a.window.Add(s)
			}
			m = a.window.Summary()
			full = a.window.Len() == battery.DefaultSamples
			if full && s.Discharging() {
				a.setStatus(fmt.Sprintf("%s - swing %s - %.1f h left - CPU %.0f%% - %d%%",
					battery.Watts(m.AvgMW), battery.Watts(m.SwingMW), m.Runtime().Hours(), cpu, m.Percent()))
			} else {
				a.setStatus(fmt.Sprintf("Measuring... (%d/%d)", a.window.Len(), battery.DefaultSamples))
			}
		}
		a.publish(s, m, full, cpu, top)

		cfg, managed := a.view()
		for _, n := range rules.Evaluate(alert.Input{
			Now:        time.Now(),
			OnBattery:  !s.AcOnline,
			Window:     m,
			WindowFull: full,
			CPU:        cpu,
			Top:        top,
			Managed:    managed,
			Cfg:        cfg,
		}) {
			a.notify(n.Title, n.Text)
		}
	}
}

// publish is the poll loop handing one tick's view of the machine to the
// rest of the tray, under ui. A discharge reading joins the history; a
// reading on AC is not a reading.
func (a *app) publish(s battery.State, m battery.Measurement, full bool, cpu float64, top []procload.Proc) {
	a.ui.Lock()
	defer a.ui.Unlock()
	a.latest, a.latestFull, a.latestOnBattery = m, full, !s.AcOnline
	a.percent, a.rateMW, a.cpu, a.top = s.Percent(), s.RateMW, cpu, top
	if s.Discharging() {
		a.history = append(a.history, point{T: time.Now().UnixMilli(), MW: s.DischargeMW()})
		if len(a.history) > historyLen {
			a.history = a.history[len(a.history)-historyLen:]
		}
	}
}

// setBusy marks a service as having a toggle in flight, for the page.
func (a *app) setBusy(name string, on bool) {
	a.ui.Lock()
	defer a.ui.Unlock()
	a.busy[name] = on
}

// view is what the rules get to see: the config and the managed set with
// each service's menu state and last measured cost, copied under ui so the
// poll loop never waits on mu. The costs are copied out per service; the
// rules read the config's thresholds and flags, never its map.
func (a *app) view() (config.Config, []alert.Service) {
	a.ui.Lock()
	defer a.ui.Unlock()

	services := make([]alert.Service, 0, len(a.managed))
	for _, c := range a.managed {
		s := alert.Service{Name: c.Name, Display: displayName(c), Running: a.running[c.Name]}
		if cost, ok := a.cfg.Costs[c.Name]; ok {
			s.Cost = &cost
		}
		services = append(services, s)
	}
	return a.cfg, services
}

// setRunning records a service's state and shows it on its checkbox. A
// service the menu no longer has (rebuilt away since the policy stopped it)
// has nothing to show.
func (a *app) setRunning(name string, on bool) {
	a.ui.Lock()
	defer a.ui.Unlock()

	item, ok := a.items[name]
	if !ok {
		return
	}
	a.running[name] = on
	if on {
		item.Check()
	} else {
		item.Uncheck()
	}
}

// notify logs a notification for the page and, if the switch is on and the
// day's cap is not spent, shows it as a toast. The log is always written:
// the dashboard's list is a record, not an interruption. The notice that
// spends the last of the cap is shown, and one marker after it says the
// rest of the day is log-only. A failure to show goes to the result line: a
// tray that cannot speak must at least say so where the user looks.
//
// The count lives in memory, keyed by the local calendar date, and a restart
// resets it. That is deliberate: the cap exists to stop a bad day flooding
// the screen, not to be an audit.
func (a *app) notify(title, text string) {
	now := time.Now()
	a.ui.Lock()
	if day := now.Format("2006-01-02"); a.toastDay != day {
		a.toastDay, a.toastCount = day, 0
	}
	shown := a.cfg.Notifications && a.toastCount < a.cfg.MaxToastsPerDay
	a.log(notice{When: now.Format("15:04:05"), Title: title, Text: text, Shown: shown})
	var marker *notice
	if shown {
		a.toastCount++
		if a.toastCount == a.cfg.MaxToastsPerDay {
			marker = &notice{
				When:  now.Format("15:04:05"),
				Title: "Notifications paused",
				Text:  fmt.Sprintf("%d today - further alerts are logged in the dashboard only until tomorrow.", a.toastCount),
				Shown: true,
			}
			a.log(*marker)
		}
	}
	a.ui.Unlock()

	if !shown {
		return
	}
	if err := notify.Show(title, text); err != nil {
		a.setResult(err.Error())
	}
	if marker != nil {
		if err := notify.Show(marker.Title, marker.Text); err != nil {
			a.setResult(err.Error())
		}
	}
}

// log prepends one entry to the page's list, under ui.
func (a *app) log(n notice) {
	a.notices = append([]notice{n}, a.notices...)
	if len(a.notices) > noticesLen {
		a.notices = a.notices[:noticesLen]
	}
}

// setStatus puts the same text on the status line and the icon's tooltip.
func (a *app) setStatus(text string) {
	a.ui.Lock()
	defer a.ui.Unlock()
	a.statusText = text
	a.status.SetTitle(text)
	systray.SetTooltip(text)
}

func (a *app) setResult(text string) {
	a.ui.Lock()
	defer a.ui.Unlock()
	a.resultText = text
	a.result.SetTitle(text)
}
