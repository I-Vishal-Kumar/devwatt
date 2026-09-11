// Package traywin finds the hidden window energye/systray creates for a tray
// icon — this process's own, for talking to its icon, or another
// devwatt-tray's, for handing a launch off to it.
//
// The window class is "SystrayClass" (systray v1.0.3, systray_windows.go:416),
// a name every systray-based program on the desktop shares, so the class alone
// identifies nothing: the caller says which process it wants. The walk is
// EnumWindows rather than FindWindow for the same reason. It never stops
// early, because EnumWindows reports a callback returning 0 as its own
// failure; the first match is kept and the walk runs out on its own.
package traywin

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

const className = "SystrayClass"

// ErrNotFound means no window of the class belongs to a process the caller
// asked for. For a launcher looking for an earlier instance that is the
// normal answer; for a process looking for its own icon it is a fault.
var ErrNotFound = errors.New("no tray window found")

// The walk writes its answer to package state under a lock rather than
// through the callback's lParam: a Go callback cannot turn a uintptr back
// into a pointer without tripping the unsafe-pointer rules. The callback is
// created once for the process, because x/sys callbacks are never released.
var (
	enumMu    sync.Mutex
	enumMatch func(pid uint32) bool
	enumFound windows.HWND
	enumProc  = windows.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		if enumFound != 0 {
			return 1
		}
		var cls [256]uint16
		n, err := windows.GetClassName(hwnd, &cls[0], int32(len(cls)))
		if err != nil || windows.UTF16ToString(cls[:n]) != className {
			return 1
		}
		var pid uint32
		windows.GetWindowThreadProcessId(hwnd, &pid)
		if enumMatch(pid) {
			enumFound = hwnd
		}
		return 1
	})
)

// Find returns the SystrayClass top-level window whose owning process
// satisfies match. The class is checked first, so match is asked only about
// the handful of tray windows on the desktop and may be as costly as opening
// the process.
func Find(match func(pid uint32) bool) (windows.HWND, error) {
	enumMu.Lock()
	defer enumMu.Unlock()

	enumMatch, enumFound = match, 0
	if err := windows.EnumWindows(enumProc, nil); err != nil {
		return 0, fmt.Errorf("EnumWindows: %w", err)
	}
	if enumFound == 0 {
		return 0, ErrNotFound
	}
	return enumFound, nil
}
