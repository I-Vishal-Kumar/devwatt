// Package notify shows a Windows notification from the tray icon.
//
// It rides the icon energye/systray already owns rather than adding one: a
// second NOTIFYICONDATA would be a second tray icon, and the WinRT toast API
// needs COM activation and an AppUserModelID for no gain, because Windows 10
// and 11 render a balloon as a toast in the notification centre anyway.
// Riding the icon means matching how the library registered it, which is a
// coupling to systray v1.0.3 and is pinned as such: the window is the one
// traywin finds for this process, and the icon's uID on it is 100
// (systray_windows.go:511).
package notify

import (
	"fmt"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/I-Vishal-Kumar/devwatt/internal/traywin"
)

const (
	iconID = 100

	nimModify = 0x00000001
	nifInfo   = 0x00000010
	niifInfo  = 0x00000001
)

// notifyIconData is NOTIFYICONDATAW from shellapi.h, field for field.
// systray's own copy splits the uTimeout/uVersion union into two fields,
// which shifts everything after szInfo, so it cannot be borrowed for a
// balloon: the title and flags would land in the wrong place.
type notifyIconData struct {
	Size            uint32
	Wnd             windows.HWND
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            windows.Handle
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	TimeoutVersion  uint32 // one DWORD in C: uTimeout, or uVersion for NIM_SETVERSION
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GuidItem        windows.GUID
	BalloonIcon     windows.Handle
}

// Show puts up a notification with the given title and text. Both are
// truncated to what the balloon can carry (63 and 255 UTF-16 units).
func Show(title, text string) error {
	pid := windows.GetCurrentProcessId()
	hwnd, err := traywin.Find(func(p uint32) bool { return p == pid })
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	nid := notifyIconData{Wnd: hwnd, ID: iconID, Flags: nifInfo, InfoFlags: niifInfo}
	nid.Size = uint32(unsafe.Sizeof(nid))
	fill(nid.InfoTitle[:], title)
	fill(nid.Info[:], text)
	r, _, err := procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
	if r == 0 {
		return fmt.Errorf("Shell_NotifyIcon(NIM_MODIFY): %w", err)
	}
	return nil
}

// fill copies s into a fixed UTF-16 buffer, cut to leave the terminating
// NUL Windows requires.
func fill(dst []uint16, s string) {
	n := copy(dst[:len(dst)-1], utf16.Encode([]rune(s)))
	dst[n] = 0
}

var (
	shell32             = windows.NewLazySystemDLL("shell32.dll")
	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")
)
