// Package helper is the elevated half of devwatt: a windowless process that
// stops and starts services on behalf of the unelevated tray, over a named
// pipe.
//
// Why a split: an elevated window cannot receive input from medium-integrity
// processes (UIPI), and that broke the keyboard vendor's Fn-key handling
// whenever the dashboard had focus. So the tray, the dashboard and every
// measurement run as the user, and only the two calls that need
// administrator rights cross into this process.
//
// Why no prompt: the helper is the scheduled task devwatt-helper, registered
// with highest privileges by an elevated `devwatt install`. A task the user
// owns runs at its registered level when the user starts it with
// `schtasks /Run`, from any integrity level, without UAC — the consent was
// given once, at install. The task starts the helper at logon; Client starts
// it on demand when it is not running.
//
// Threat model: the pipe admits SYSTEM, Administrators and this user, and no
// one else. Any process running as this user can therefore stop or start an
// allowlisted service without a prompt — the same trust the logon task
// already grants that user — and nothing beyond that: the helper hands the
// name to control, which re-checks catalog.Denied under the pipe, so the
// tray's UI is not the only gate.
//
// Protocol: one request per connection, one line each way. `stop <name>` or
// `start <name>`; the reply is `ok` or `error <text>`.
package helper

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/I-Vishal-Kumar/devwatt/internal/autostart"
	"github.com/I-Vishal-Kumar/devwatt/internal/control"
	"github.com/I-Vishal-Kumar/devwatt/internal/elevate"
)

// pipe is the helper's endpoint.
const pipe = `\\.\pipe\devwatt-control`

// ErrAlreadyRunning means another helper already serves the pipe; the new
// one has nothing to do and should exit quietly.
var ErrAlreadyRunning = errors.New("a helper is already serving " + pipe)

const (
	requestTimeout = 5 * time.Second  // a stalled client must not wedge the helper
	replyTimeout   = 5 * time.Second  // set after the act, which itself can take control's 30 s
	dialTimeout    = 2 * time.Second  // one connection attempt
	startWait      = 10 * time.Second // for the task to bring the pipe up
	startPoll      = 250 * time.Millisecond
	// The reply can take as long as control waits for a service, plus the
	// settle a caller adds; the client waits generously.
	clientTimeout = 60 * time.Second
)

// Serve runs the helper until it fails. It must already be elevated —
// that is the task's job, not this process's.
func Serve() error {
	if !elevate.Elevated() {
		return errors.New("the helper must run elevated; it is started by the " + autostart.HelperTaskName + " task")
	}
	sd, err := pipeSecurity()
	if err != nil {
		return err
	}
	l, err := winio.ListenPipe(pipe, &winio.PipeConfig{SecurityDescriptor: sd})
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// The first instance of a pipe name can be created once; a second
		// creator is refused. That is the running helper.
		return ErrAlreadyRunning
	}
	if err != nil {
		return fmt.Errorf("helper: listen: %w", err)
	}
	defer l.Close()

	// One request at a time: control is serialised on the tray's side too,
	// and two acts on one service at once would be a race worth avoiding.
	for {
		conn, err := l.Accept()
		if err != nil {
			return fmt.Errorf("helper: accept: %w", err)
		}
		serveOne(conn)
	}
}

// pipeSecurity is the DACL the pipe is created with: full access to SYSTEM,
// Administrators and the user this helper runs as.
func pipeSecurity() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("helper: GetTokenUser: %w", err)
	}
	return fmt.Sprintf("D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)", u.User.Sid.String()), nil
}

func serveOne(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(requestTimeout))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return // the client went away or stalled; nothing to answer
	}
	verb, name, _ := strings.Cut(strings.TrimSpace(line), " ")
	switch {
	case verb == "stop" && name != "":
		err = control.Stop(name)
	case verb == "start" && name != "":
		err = control.Start(name)
	default:
		err = fmt.Errorf("malformed request %q", strings.TrimSpace(line))
	}
	conn.SetWriteDeadline(time.Now().Add(replyTimeout))
	if err != nil {
		fmt.Fprintf(conn, "error %s\n", err)
		return
	}
	fmt.Fprintln(conn, "ok")
}

// Client is the tray's cost.Controller: each call is one request to the
// helper, starting it first if it is not running.
type Client struct{}

func (Client) Stop(name string) error  { return request("stop " + name) }
func (Client) Start(name string) error { return request("start " + name) }

func request(line string) error {
	conn, err := dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(clientTimeout))
	if _, err := fmt.Fprintln(conn, line); err != nil {
		return fmt.Errorf("helper: %w", err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("helper: %w", err)
	}
	reply = strings.TrimSpace(reply)
	switch {
	case reply == "ok":
		return nil
	case strings.HasPrefix(reply, "error "):
		return errors.New(strings.TrimPrefix(reply, "error "))
	default:
		return fmt.Errorf("helper: unexpected reply %q", reply)
	}
}

// dial connects to the helper. When the pipe does not exist the helper is
// not running, so its task is started and the pipe is polled until it
// appears or the wait runs out.
func dial() (net.Conn, error) {
	timeout := dialTimeout
	conn, err := winio.DialPipe(pipe, &timeout)
	if err == nil {
		return conn, nil
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, fmt.Errorf("helper: %w", err)
	}
	if err := autostart.Start(autostart.HelperTaskName); err != nil {
		return nil, fmt.Errorf(`helper is not running and could not be started (%v); run "devwatt install" from an elevated terminal`, err)
	}
	deadline := time.Now().Add(startWait)
	for time.Now().Before(deadline) {
		time.Sleep(startPoll)
		conn, err = winio.DialPipe(pipe, &timeout)
		if err == nil {
			return conn, nil
		}
		if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return nil, fmt.Errorf("helper: %w", err)
		}
	}
	return nil, errors.New(`helper is not running and could not be started; run "devwatt install" from an elevated terminal`)
}
