// Package elevate answers whether a process holds administrator rights.
//
// The command line's cost, install and uninstall need them and refuse
// without them; the helper needs them and is started with them by its
// task; the tray must never have them, because an elevated window cannot
// take input from the user's medium-integrity programs (UIPI) — every
// keyboard, clipboard and accessibility tool goes dark for it. An elevated
// launch of the tray (an installer's finish page, "Run as administrator")
// is therefore handed to the tray's own logon task, which starts it as the
// user; see cmd/devwatt-tray.
package elevate

import "golang.org/x/sys/windows"

// Elevated reports whether this process is running with administrator rights.
func Elevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}
