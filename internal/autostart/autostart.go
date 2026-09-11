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
// The tray's own task is LeastPrivilege, which is the level the user already
// has, so the dashboard can create or remove it without elevation. Both use
// the interactive token: the tray needs a desktop for its icon, and the
// helper only matters while the user is logged on.
//
// The tasks are defined by XML rather than schtasks' /Create switches
// because the switches cannot reach the settings that matter for a battery
// tool. A task created with /Create starts only on AC power, is stopped when
// the charger comes out, and is killed after 72 hours — defaults that would
// leave the tray absent at a logon on battery and the helper unstartable
// exactly when a service is being stopped to save power.
package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
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
	return create(HelperTaskName, trayExe, "--helper", "HighestAvailable")
}

// InstallUser creates or replaces the tray's task alone. It runs at the
// user's own level, so no elevation is needed to create it.
func InstallUser(trayExe string) error {
	return create(TaskName, trayExe, "", "LeastPrivilege")
}

// create registers one logon task from XML. The principal is this process's
// user, by SID, so the task belongs to the account that will log on, whether
// or not the caller is elevated.
func create(name, exe, args, runLevel string) error {
	// GetCurrentProcessToken is a pseudo-handle: TOKEN_QUERY, nothing to close.
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	xml := taskXML(user.User.Sid.String(), exe, args, runLevel)

	// schtasks reads the definition from a file, and reads it as UTF-16.
	f, err := os.CreateTemp("", "devwatt-task-*.xml")
	if err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(utf16LE(xml)); err != nil {
		f.Close()
		return fmt.Errorf("autostart: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("autostart: %w", err)
	}
	return schtasks("/Create", "/XML", f.Name(), "/TN", name, "/F")
}

// taskXML is a Task Scheduler definition: a logon trigger for the user, the
// interactive token at the given run level, allowed to start and keep
// running on battery, with no execution time limit, one instance at a time.
func taskXML(sid, exe, args, runLevel string) string {
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>devwatt</Description></RegistrationInfo>
  <Triggers>
    <LogonTrigger><Enabled>true</Enabled><UserId>` + sid + `</UserId></LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author"><UserId>` + sid + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>` + runLevel + `</RunLevel></Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <StartWhenAvailable>true</StartWhenAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>4</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec><Command>` + exe + `</Command><Arguments>` + args + `</Arguments></Exec>
  </Actions>
</Task>
`
}

// utf16LE encodes s as schtasks expects a definition file: UTF-16 little
// endian with a byte-order mark.
func utf16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, 0, 2+2*len(units))
	b = append(b, 0xFF, 0xFE)
	for _, u := range units {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
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
