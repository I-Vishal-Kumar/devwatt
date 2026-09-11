package main

// One tray per user, and a way for a second launch to reach the first. Both
// run as the user now, so a plain window message crosses between them; the
// message is WM_APP+2, distinct from WM_APP+1, which is the thread message
// our own loop uses to open the dashboard from a click.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/I-Vishal-Kumar/devwatt/internal/autostart"
	"github.com/I-Vishal-Kumar/devwatt/internal/traywin"
)

// wmOpenFromLauncher is posted to a running instance's tray window by a
// second launch that was asked for the dashboard.
const wmOpenFromLauncher = wmApp + 2

// mode is what the command line asked this process to be.
type mode int

const (
	modeTray      mode = iota // the tray, as at logon
	modeDashboard             // the tray, with the dashboard open (the Start menu entry)
	modeHelper                // the elevated helper, started by its task
)

// parseArgs accepts the two arguments there are: --dashboard, which the
// Start menu shortcut passes, and --helper, which the helper's task passes.
// The tray's own logon task passes none.
func parseArgs(args []string) (mode, error) {
	m := modeTray
	for _, arg := range args {
		switch arg {
		case "--dashboard":
			m = modeDashboard
		case "--helper":
			m = modeHelper
		default:
			return modeTray, fmt.Errorf("unknown argument %q", arg)
		}
	}
	return m, nil
}

// otherInstance finds a devwatt-tray other than this process, by the tray
// window it owns: a SystrayClass window whose process runs an executable of
// the same name as ours. The helper has no window, so it is never matched.
func otherInstance() (windows.HWND, bool, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, false, err
	}
	me := strings.ToLower(filepath.Base(self))
	pid := windows.GetCurrentProcessId()
	hwnd, err := traywin.Find(func(p uint32) bool { return p != pid && imageBase(p) == me })
	if errors.Is(err, traywin.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return hwnd, true, nil
}

// imageBase is a process's executable file name, lower-cased. A process
// that cannot be asked — a protected one — cannot be shown to be ours, so
// it is not.
func imageBase(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return strings.ToLower(filepath.Base(windows.UTF16ToString(buf[:n])))
}

// startAsUser is what an elevated launch does instead of running: it starts
// the tray's own logon task — a LIMITED task the user owns, which runs the
// tray as the user from any integrity level without a prompt — waits for
// that instance's tray window, and hands it the dashboard request if there
// was one. Windows offers an elevated process no usable handle to the
// user's unelevated self (the linked token comes back at identification
// level without SeTcbPrivilege), so the task is the only way down.
func startAsUser(m mode) error {
	if err := autostart.Start(autostart.TaskName); err != nil {
		return fmt.Errorf(`devwatt is not installed for this user; run "devwatt install" from an elevated terminal, then start devwatt from Start (%v)`, err)
	}
	deadline := time.Now().Add(startAsUserWait)
	for time.Now().Before(deadline) {
		time.Sleep(startAsUserPoll)
		hwnd, running, err := otherInstance()
		if err != nil {
			return err
		}
		if !running {
			continue
		}
		if m == modeDashboard {
			return handOff(hwnd)
		}
		return nil
	}
	return fmt.Errorf("the %s task was started but no devwatt-tray window appeared within %v", autostart.TaskName, startAsUserWait)
}

const (
	startAsUserWait = 10 * time.Second
	startAsUserPoll = 250 * time.Millisecond
)

// handOff asks the running instance to open its dashboard.
func handOff(hwnd windows.HWND) error {
	if r, _, err := procPostMessage.Call(uintptr(hwnd), wmOpenFromLauncher, 0, 0); r == 0 {
		return fmt.Errorf("PostMessage to the running devwatt-tray: %w", err)
	}
	return nil
}

// acceptHandOff routes wmOpenFromLauncher on this instance's tray window to
// openDashboard. It runs on the main thread, which owns the window, so the
// subclass is installed by the thread whose procedure it replaces.
func (a *app) acceptHandOff() error {
	pid := windows.GetCurrentProcessId()
	hwnd, err := traywin.Find(func(p uint32) bool { return p == pid })
	if err != nil {
		return err
	}

	// The callback is created once, here, with the app in its closure; orig
	// is filled in below and read by the callback from then on.
	var orig uintptr
	proc := windows.NewCallback(func(hwnd, m, wp, lp uintptr) uintptr {
		if m == wmOpenFromLauncher {
			a.openDashboard()
			return 0
		}
		r, _, _ := procCallWindowProc.Call(orig, hwnd, m, wp, lp)
		return r
	})
	orig, _, err = procSetWindowLongPtr.Call(uintptr(hwnd), gwlpWndProc, proc)
	if orig == 0 {
		return fmt.Errorf("SetWindowLongPtr(GWLP_WNDPROC) on the tray window: %w", err)
	}
	return nil
}

var procPostMessage = user32.NewProc("PostMessageW")
