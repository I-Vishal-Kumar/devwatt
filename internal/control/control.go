// Package control stops, starts and queries the services devwatt manages.
//
// This is the only package that changes a service's state, so it is where
// the guarantees live. The catalog's veto is checked again here even though
// every caller should already have checked it: catalog.go promises that gate
// sits under everything, and a widened pattern or a UI that forgot must not
// be enough to reach a denied service. Each handle is opened with the one
// right its operation needs, plus the right to ask the state, and nothing
// more; SERVICE_STOP and SERVICE_START are granted only to elevated callers,
// so an unelevated process is refused at OpenService, before any state is
// touched.
//
// Stop and Start do not return until the service reports the state it was
// asked for. A stop request is accepted immediately and the service winds
// down on its own time (a database flushing its buffer pool can take
// seconds); a caller measuring the cost of that service must not start
// sampling until the process is actually gone.
package control

import (
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/I-Vishal-Kumar/devwatt/internal/catalog"
	"github.com/I-Vishal-Kumar/devwatt/internal/scm"
)

// Polling the service for its target state. 250 ms is fine enough that a
// quick service does not pad the measurement, and 30 s is longer than any
// well-behaved developer service needs to stop or start, so a service that
// blows it is reported as stuck rather than waited on forever.
const (
	pollInterval      = 250 * time.Millisecond
	transitionTimeout = 30 * time.Second
)

// Stop stops a running service and returns once it reports Stopped. A service
// already stopped is left alone.
func Stop(name string) error {
	return transition(name, "stop", windows.SERVICE_STOP, svc.Stopped, func(s *mgr.Service) error {
		_, err := s.Control(svc.Stop)
		return err
	})
}

// Start starts a stopped service and returns once it reports Running. A
// service already running is left alone.
func Start(name string) error {
	return transition(name, "start", windows.SERVICE_START, svc.Running, func(s *mgr.Service) error {
		return s.Start()
	})
}

// transition is the shape Stop and Start share: veto, open with the one right
// that can do the job, skip if already there, act, then wait for the service
// to say so.
func transition(name, verb string, right uint32, target svc.State, act func(*mgr.Service) error) error {
	if catalog.Denied(name) {
		return fmt.Errorf("refusing to %s %s: it is on the denied list", verb, name)
	}

	m, err := scm.OpenManager(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return err
	}
	defer m.Disconnect()

	s, err := scm.OpenService(m, name, right|windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return err
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("query %s: %w", name, err)
	}
	if st.State == target {
		return nil
	}

	if err := act(s); err != nil {
		return fmt.Errorf("%s %s: %w", verb, name, err)
	}

	deadline := time.Now().Add(transitionTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)
		st, err = s.Query()
		if err != nil {
			return fmt.Errorf("query %s: %w", name, err)
		}
		if st.State == target {
			return nil
		}
	}
	return fmt.Errorf("%s %s: still %s after %v", verb, name, stateName(st.State), transitionTimeout)
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start pending"
	case svc.StopPending:
		return "stop pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue pending"
	case svc.PausePending:
		return "pause pending"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("state %d", s)
	}
}

// Direct is a cost.Controller for a process that holds the rights itself:
// the command line in an elevated terminal. The tray does not; it goes
// through the helper.
type Direct struct{}

func (Direct) Stop(name string) error  { return Stop(name) }
func (Direct) Start(name string) error { return Start(name) }

// Running reports whether a service is currently in the Running state. It
// exists so a caller that has just toggled one service can re-check that one
// service, instead of enumerating every service on the machine to learn the
// state of one.
func Running(name string) (bool, error) {
	if catalog.Denied(name) {
		return false, fmt.Errorf("refusing to query %s: it is on the denied list", name)
	}

	m, err := scm.OpenManager(windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false, err
	}
	defer m.Disconnect()

	s, err := scm.OpenService(m, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false, err
	}
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return false, fmt.Errorf("query %s: %w", name, err)
	}
	return st.State == svc.Running, nil
}
