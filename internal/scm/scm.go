// Package scm opens the service control manager and individual services with
// exactly the rights the caller names, and no more.
//
// The x/sys helpers are the wrong shape for devwatt. mgr.Connect asks for
// SC_MANAGER_ALL_ACCESS and mgr.OpenService for SERVICE_ALL_ACCESS, and both
// fail unelevated. Discovery has to work from an ordinary terminal, and
// control should hold nothing it does not use, so every open here takes its
// rights explicitly and returns the x/sys types so callers keep Config,
// Query, Control and Start without a second wrapper.
package scm

import (
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// OpenManager connects to the local service database with the given
// SC_MANAGER_* rights. The caller releases it with Disconnect.
func OpenManager(rights uint32) (*mgr.Mgr, error) {
	h, err := windows.OpenSCManager(nil, nil, rights)
	if err != nil {
		return nil, fmt.Errorf("OpenSCManager: %w", err)
	}
	return &mgr.Mgr{Handle: h}, nil
}

// OpenService opens one service by name with the given SERVICE_* rights. The
// caller releases it with Close.
func OpenService(m *mgr.Mgr, name string, rights uint32) (*mgr.Service, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.OpenService(m.Handle, p, rights)
	if err != nil {
		return nil, fmt.Errorf("OpenService(%s): %w", name, err)
	}
	return &mgr.Service{Name: name, Handle: h}, nil
}
