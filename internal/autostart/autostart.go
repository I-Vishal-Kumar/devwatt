// Package autostart registers devwatt's two logon tasks: the tray, which
// runs as the user, and the helper, which runs elevated.
//
// The Run key is the usual answer and the wrong one here. The helper needs
// administrator rights, so a Run-key entry would mean a UAC prompt at every
// logon — which users learn to dismiss, and then nothing is running. A
// scheduled task with RunLevel Highest starts elevated without a prompt:
// the consent was given once, when the task was created from an elevated
// `devwatt install`. The same task is how the tray starts the helper on
// demand: a task the user owns runs at its registered level when the user
// starts it with `schtasks /Run`, from any integrity level, without UAC.
// The tray's own task is LIMITED, which is the level the user already has,
// so the dashboard can create or remove it without elevation. Both are
// interactive (/IT): the tray needs a desktop for its icon, and the helper
// only matters while the user is logged on.
package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Task names, as Task Scheduler shows them.
const (
	TaskName       = "devwatt"        // the tray, as the user
	HelperTaskName = "devwatt-helper" // the elevated helper
)

// Install creates or replaces both logon tasks for the given executable.
// It needs an elevated caller for the helper's task.
func Install(trayExe string) error {
	if err := InstallUser(trayExe); err != nil {
		return err
	}
	// schtasks parses the /TR value again itself, so the path is quoted
	// inside the value and the argument follows the closing quote.
	return schtasks("/Create", "/SC", "ONLOGON", "/RL", "HIGHEST", "/IT",
		"/TN", HelperTaskName, "/TR", `"`+trayExe+`" --helper`, "/F")
}

// InstallUser creates or replaces the tray's task alone. It runs at the
// user's own level, so no elevation is needed to create it.
func InstallUser(trayExe string) error {
	return schtasks("/Create", "/SC", "ONLOGON", "/RL", "LIMITED", "/IT",
		"/TN", TaskName, "/TR", `"`+trayExe+`"`, "/F")
}

// Uninstall removes both logon tasks.
func Uninstall() error {
	if err := UninstallUser(); err != nil {
		return err
	}
	return schtasks("/Delete", "/TN", HelperTaskName, "/F")
}

// UninstallUser removes the tray's task alone.
func UninstallUser() error {
	return schtasks("/Delete", "/TN", TaskName, "/F")
}

// Start runs a task now, at the level it was registered with.
func Start(task string) error {
	return schtasks("/Run", "/TN", task)
}

// Installed reports whether the tray's logon task exists. Task Scheduler
// keeps every task's definition as an XML file under System32\Tasks with an
// ACL that grants its author read, so a task this user registered is visible
// from an unelevated process; the registry cache it also keeps is not. No
// COM, and nothing localised to parse.
func Installed() (bool, error) {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return false, errors.New("SystemRoot is not set")
	}
	_, err := os.Stat(filepath.Join(systemRoot, "System32", "Tasks", TaskName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("autostart: %w", err)
	}
	return true, nil
}

// schtasks runs the system's schtasks.exe by its full path — an executable
// found through PATH is not something to run from an elevated caller — and
// turns a failure into an error carrying what it printed.
func schtasks(args ...string) error {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return errors.New("SystemRoot is not set")
	}
	exe := filepath.Join(systemRoot, "System32", "schtasks.exe")
	out, err := exec.Command(exe, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("schtasks %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}
