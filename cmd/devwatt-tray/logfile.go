package main

// The tray has no console, and its fatal box can appear behind a logon in
// progress and go unseen for hours while the process lives on with nothing
// initialised. So both roles of this executable keep a log next to the
// config: what started, what failed, and every result line the tray showed.
// It is the one place a failure at logon can be read back from.

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// logLimit is the size past which the log starts over on the next launch: a
// tray that reports an error every tick must not grow a file without bound.
const logLimit = 1 << 20

// openLog sends the standard logger to %APPDATA%\devwatt\devwatt-tray.log.
func openLog(role string) error {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return errors.New("APPDATA is not set")
	}
	dir := filepath.Join(appData, "devwatt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("log: %w", err)
	}
	path := filepath.Join(dir, "devwatt-tray.log")
	flags := os.O_APPEND | os.O_CREATE | os.O_WRONLY
	if fi, err := os.Stat(path); err == nil && fi.Size() > logLimit {
		flags = os.O_TRUNC | os.O_CREATE | os.O_WRONLY
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return fmt.Errorf("log: %w", err)
	}
	log.SetOutput(f)
	log.SetPrefix(role + " ")
	return nil
}
