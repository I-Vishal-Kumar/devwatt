// Package shortcut puts devwatt in the Start menu, so the dashboard can be
// opened by typing its name.
//
// A .lnk is written through IShellLink, which is COM; x/sys has no
// CoCreateInstance, and a vtable binding for the sake of one shortcut is
// not worth its weight. PowerShell's WScript.Shell does the same call and
// is the system's own tool, as schtasks is for the logon task. Every path
// goes into the script single-quoted with embedded quotes doubled, which is
// PowerShell's literal string.
package shortcut

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Name is the entry as the Start menu shows it.
const Name = "devwatt"

// Install writes the shortcut: target run with args from target's own
// directory, showing iconPath's first icon.
func Install(target, args, iconPath string) error {
	lnk, err := path()
	if err != nil {
		return err
	}
	script := fmt.Sprintf(
		"$s = (New-Object -ComObject WScript.Shell).CreateShortcut(%s); "+
			"$s.TargetPath = %s; $s.Arguments = %s; $s.WorkingDirectory = %s; "+
			"$s.IconLocation = %s; $s.Description = %s; $s.Save()",
		quote(lnk), quote(target), quote(args), quote(filepath.Dir(target)),
		quote(iconPath+",0"), quote("devwatt - battery dashboard"))
	return powershell(script)
}

// Uninstall removes the shortcut. A shortcut that is already gone is the
// state asked for, not a failure.
func Uninstall() error {
	lnk, err := path()
	if err != nil {
		return err
	}
	if err := os.Remove(lnk); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("shortcut: %w", err)
	}
	return nil
}

// path is the per-user Start menu entry. APPDATA missing is an error, not a
// cue to guess a directory.
func path() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", errors.New("APPDATA is not set")
	}
	return filepath.Join(appData, `Microsoft\Windows\Start Menu\Programs`, Name+".lnk"), nil
}

// quote is a PowerShell single-quoted literal of s.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// powershell runs one command in the system's Windows PowerShell by its full
// path — this runs elevated, and an executable found through PATH is not
// something to run elevated — and turns a failure into an error carrying
// what it printed.
func powershell(command string) error {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return errors.New("SystemRoot is not set")
	}
	exe := filepath.Join(systemRoot, `System32\WindowsPowerShell\v1.0\powershell.exe`)
	out, err := exec.Command(exe, "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
	if err != nil {
		return fmt.Errorf("powershell: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
