// Command devwatt is the command line: it lists the services devwatt is
// willing to manage, measures what the machine is drawing, prices a service
// by stopping or starting it between two measurements, and registers the
// tray to start at logon.
//
// The measuring and the service control behind the tray are reachable from
// here, so each can be checked on a real machine from a terminal, where the
// numbers are visible.
//
//	devwatt                            list managed and suggested services
//	devwatt measure                    sample the battery and report sustained draw
//	devwatt cost <service> stop|start  stop or start a service and report what it changed
//	devwatt install                    start devwatt-tray and its elevated helper at logon, and add it to the Start menu
//	devwatt uninstall                  remove the logon tasks and the Start menu entry
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/I-Vishal-Kumar/devwatt/internal/autostart"
	"github.com/I-Vishal-Kumar/devwatt/internal/battery"
	"github.com/I-Vishal-Kumar/devwatt/internal/control"
	"github.com/I-Vishal-Kumar/devwatt/internal/cost"
	"github.com/I-Vishal-Kumar/devwatt/internal/discover"
	"github.com/I-Vishal-Kumar/devwatt/internal/elevate"
	"github.com/I-Vishal-Kumar/devwatt/internal/icon"
	"github.com/I-Vishal-Kumar/devwatt/internal/shortcut"
)

func main() {
	cmd := "list"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	var err error
	switch cmd {
	case "list":
		err = list()
	case "measure":
		err = measure()
	case "cost":
		if len(os.Args) != 4 {
			usage("cost takes a service name and an action")
		}
		action, ok := parseAction(os.Args[3])
		if !ok {
			usage(fmt.Sprintf("unknown action %q (want stop or start)", os.Args[3]))
		}
		err = costCmd(os.Args[2], action)
	case "install":
		err = install()
	case "uninstall":
		err = uninstall()
	default:
		fmt.Fprintf(os.Stderr, "devwatt: unknown command %q (want list, measure, cost, install or uninstall)\n", cmd)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "devwatt: %v\n", err)
		os.Exit(1)
	}
}

func list() error {
	cands, err := discover.List()
	if err != nil {
		return err
	}

	var known, suggested []discover.Candidate
	for _, c := range cands {
		if c.InCatalog {
			known = append(known, c)
		} else {
			suggested = append(suggested, c)
		}
	}
	sort.Slice(known, func(i, j int) bool { return known[i].Name < known[j].Name })
	sort.Slice(suggested, func(i, j int) bool { return suggested[i].Name < suggested[j].Name })

	fmt.Printf("MANAGED (%d) - recognised, safe to enable by default\n", len(known))
	fmt.Println("------------------------------------------------------------")
	for _, c := range known {
		fmt.Printf("  %-24s %-8s %-10s %s - %s\n", c.Name, state(c), c.StartType, c.Entry.Display, c.Entry.Note)
	}
	if len(known) == 0 {
		fmt.Println("  (none)")
	}

	// The binary path is what decides whether to adopt something called
	// "SmartConnect"; the display name is what the tray shows, so it lives
	// there.
	fmt.Printf("\nSUGGESTED (%d) - require explicit opt-in, never auto-enabled\n", len(suggested))
	fmt.Println("------------------------------------------------------------")
	for _, c := range suggested {
		fmt.Printf("  %-34s %-8s %-10s %s\n", trunc(c.Name, 34), state(c), c.StartType, c.BinaryPath)
	}
	return nil
}

// measure samples the battery and prints the sustained draw with its swing,
// the CPU load over the same window, and the runtime that draw implies.
//
// It refuses to print a number it cannot stand behind: on AC there is no
// discharge to read, and a wide swing is flagged rather than averaged away.
func measure() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := printBattery(); err != nil {
		return err
	}
	fmt.Printf("Sampling %d readings %v apart...\n", battery.DefaultSamples, battery.DefaultInterval)

	m, cpu, err := cost.Sample(ctx)
	if err != nil {
		return interrupted(err)
	}
	printMeasurement(m, cpu)
	return nil
}

// costCmd stops or starts one service between two measurements and prints
// both sides in full, so the user sees the confounders and not just the
// verdict.
//
// It needs an elevated terminal. Relaunching under UAC from inside a command
// that prints to this terminal would put the output in a window that closes
// when it finishes, so the precondition is checked and reported instead.
func costCmd(service string, action cost.Action) error {
	if err := requireElevated("cost", "stopping and starting services"); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := printBattery(); err != nil {
		return err
	}
	fmt.Printf("Sampling %d readings %v apart, then %s %s, settling %v, then sampling again...\n",
		battery.DefaultSamples, battery.DefaultInterval, action.Progressive(), service, cost.SettleTime)

	obs := &costObserver{service: service, action: action}
	r, err := cost.Run(ctx, service, action, obs, control.Direct{})
	if err != nil {
		if errors.Is(err, context.Canceled) && obs.acted {
			return fmt.Errorf("interrupted after %s was %s; it stays %s, measurement discarded", service, action, action)
		}
		return interrupted(err)
	}

	fmt.Println("AFTER")
	printMeasurement(r.After, r.CPUAfter)
	fmt.Println(r)
	return nil
}

// costObserver prints each stage of a cost run as it lands, and remembers
// whether the service was touched so an interrupt can say so instead of
// pretending nothing happened.
type costObserver struct {
	service string
	action  cost.Action
	acted   bool
}

func (o *costObserver) Baseline(m battery.Measurement, cpu float64) {
	fmt.Println("BEFORE")
	printMeasurement(m, cpu)
}

func (o *costObserver) Transitioned(took time.Duration) {
	o.acted = true
	fmt.Printf("%s: %s... %s in %.1fs, settling %v\n", o.service, o.action.Progressive(), o.action, took.Seconds(), cost.SettleTime)
}

// install registers the two logon tasks — the tray as the user, the helper
// elevated and without a prompt — and puts the tray in the Start menu with
// its icon so the dashboard is a search away. It expects devwatt-tray.exe
// next to this executable: the two are built together and shipped
// together, and a task pointing at a path that does not exist would fail
// silently at the next logon.
func install() error {
	if err := requireElevated("install", "registering a logon task with highest privileges"); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(self)
	trayExe := filepath.Join(dir, "devwatt-tray.exe")
	if _, err := os.Stat(trayExe); err != nil {
		return fmt.Errorf("devwatt-tray.exe not found next to devwatt.exe (looked in %s)", dir)
	}
	if err := autostart.Install(trayExe); err != nil {
		return err
	}
	fmt.Printf("registered scheduled task %q: %s at logon, as you\n", autostart.TaskName, trayExe)
	fmt.Printf("registered scheduled task %q: %s --helper at logon, highest privileges\n", autostart.HelperTaskName, trayExe)

	// The shortcut's icon has to be a file; the same bytes the tray embeds
	// go next to it.
	icoPath := filepath.Join(dir, "devwatt.ico")
	if err := os.WriteFile(icoPath, icon.Bytes, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", icoPath)

	if err := shortcut.Install(trayExe, "--dashboard", icoPath); err != nil {
		return err
	}
	fmt.Printf("added Start menu entry %q\n", shortcut.Name)
	return nil
}

func uninstall() error {
	if err := requireElevated("uninstall", "removing a logon task"); err != nil {
		return err
	}
	if err := autostart.Uninstall(); err != nil {
		return err
	}
	fmt.Printf("removed scheduled tasks %q and %q\n", autostart.TaskName, autostart.HelperTaskName)

	if err := shortcut.Uninstall(); err != nil {
		return err
	}
	fmt.Printf("removed Start menu entry %q\n", shortcut.Name)

	self, err := os.Executable()
	if err != nil {
		return err
	}
	icoPath := filepath.Join(filepath.Dir(self), "devwatt.ico")
	if err := os.Remove(icoPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Printf("removed %s\n", icoPath)
	return nil
}

// requireElevated is the precondition for every command that changes the
// machine. Relaunching under UAC from inside a command that prints to this
// terminal would put the output in a window that closes when it finishes,
// so the terminal itself has to be elevated, and the error says so.
func requireElevated(cmd, what string) error {
	if elevate.Elevated() {
		return nil
	}
	return fmt.Errorf("%s needs an elevated terminal (%s requires administrator rights)", cmd, what)
}

// printBattery prints the charge line that heads every measurement, and
// refuses to go on when there is no discharge to read.
func printBattery() error {
	s, err := battery.Read()
	if err != nil {
		return err
	}
	if s.AcOnline {
		return battery.ErrOnAC
	}
	fmt.Printf("Battery  %d%%  %d of %d mWh, discharging\n", s.Percent(), s.RemainingMWh, s.FullMWh)
	return nil
}

// printMeasurement is the block measure prints and cost prints twice: every
// reading, so a burst that settled is seen rather than averaged away; the
// swing with its verdict; the CPU confounder; and the runtime the draw implies.
func printMeasurement(m battery.Measurement, cpu float64) {
	readings := make([]string, len(m.ReadingsMW))
	for i, r := range m.ReadingsMW {
		readings[i] = fmt.Sprint(r)
	}
	fmt.Printf("  readings  %s mW\n", strings.Join(readings, " "))
	fmt.Printf("  AVG       %d mW\n", m.AvgMW)
	fmt.Printf("  SWING     %d mW  (min %d, max %d)  %s\n", m.SwingMW, m.MinMW, m.MaxMW, verdict(m))
	fmt.Printf("  CPU       %.1f%% busy over the window\n", cpu)
	fmt.Printf("  RUNTIME   %.2f h at this draw  (%d mWh remaining)\n", m.Runtime().Hours(), m.RemainingMWh)
}

func verdict(m battery.Measurement) string {
	if m.Trustworthy() {
		return "steady"
	}
	return fmt.Sprintf("UNSTABLE - something is bursting; find it before trusting the average (threshold %d mW)", battery.TrustworthySwingMW)
}

// interrupted rewords a cancelled context as the Ctrl+C it was.
func interrupted(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("interrupted")
	}
	return err
}

// usage reports a malformed cost invocation and exits 2, as an unknown
// command does.
func usage(reason string) {
	fmt.Fprintf(os.Stderr, "devwatt: %s\nusage: devwatt cost <service> stop|start\n", reason)
	os.Exit(2)
}

func parseAction(word string) (cost.Action, bool) {
	switch word {
	case "stop":
		return cost.Stop, true
	case "start":
		return cost.Start, true
	}
	return 0, false
}

func state(c discover.Candidate) string {
	if c.Running {
		return "running"
	}
	return "stopped"
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
