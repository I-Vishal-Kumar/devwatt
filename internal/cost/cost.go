// Package cost prices one service by measuring the battery before and after
// stopping or starting it.
//
// This is the A/B protocol from the triage method, and its discipline is the
// whole point. A delta between two measurements means nothing on its own:
// draw jitters by hundreds of milliwatts at rest, a process winding down is a
// burst, and anything else that changed on the machine between the two sides
// is folded into the number. So Run records the CPU load of both sides as a
// confounder, waits out the burst before sampling, and the Result judges
// whether the comparison can be believed before it prints a delta. A void
// result is reported as void, never as a number; a brightness A/B once came
// back "brighter saves power" because an update worker exited between the
// two samples.
package cost

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/I-Vishal-Kumar/devwatt/internal/battery"
	"github.com/I-Vishal-Kumar/devwatt/internal/cpuload"
)

// Action is what Run does to the service between the two measurements.
type Action int

const (
	Stop Action = iota + 1
	Start
)

// String is the past tense, for the result line.
func (a Action) String() string {
	switch a {
	case Stop:
		return "stopped"
	case Start:
		return "started"
	default:
		return fmt.Sprintf("Action(%d)", int(a))
	}
}

// Progressive is the in-progress form, for a line that says what is
// happening right now.
func (a Action) Progressive() string {
	switch a {
	case Stop:
		return "stopping"
	case Start:
		return "starting"
	default:
		return fmt.Sprintf("Action(%d)", int(a))
	}
}

const (
	// SettleTime is how long Run waits after the service reports its new
	// state before sampling again. Process teardown and startup are bursts —
	// a database flushing buffers, a JVM warming up — and the controller would
	// report them as the service's cost. Sampling must not begin until it
	// passes.
	SettleTime = 5 * time.Second

	// maxCPUDrift is the CPU change, in percentage points, beyond which the
	// two sides are not comparable. The rule is the triage method's: if a
	// confounder moved more than the effect, the test is void. Five points of
	// CPU on a laptop is on the order of watts, which is bigger than any idle
	// service.
	maxCPUDrift = 5.0
)

// Result is one A/B run: both sides in full, so the caller can show the
// confounders and not just the delta.
type Result struct {
	Service string
	Action  Action

	Before, After       battery.Measurement
	CPUBefore, CPUAfter float64
}

// DeltaMW is the change in sustained draw. Negative is a saving.
func (r Result) DeltaMW() int {
	return r.After.AvgMW - r.Before.AvgMW
}

// Void reports whether the comparison cannot be trusted, and why: either side
// was unstable on its own, or the CPU load moved between them by more than
// maxCPUDrift. The reason is what gets remembered with the result, so a later
// "last measured" line can say why there is no number.
func (r Result) Void() (reason string, void bool) {
	if !r.Before.Trustworthy() {
		return fmt.Sprintf("before was unstable (swing %s mW)", commas(r.Before.SwingMW)), true
	}
	if !r.After.Trustworthy() {
		return fmt.Sprintf("after was unstable (swing %s mW)", commas(r.After.SwingMW)), true
	}
	if math.Abs(r.CPUAfter-r.CPUBefore) > maxCPUDrift {
		return fmt.Sprintf("CPU moved %s", cpuRange(r.CPUBefore, r.CPUAfter)), true
	}
	return "", false
}

// String is the one-line verdict the CLI and the tray both print, for
// example:
//
//	postgresql-x64-17 stopped: -1,100 mW (21,800 -> 20,700 mW, CPU 30->29%)
//	postgresql-x64-17 stopped: VOID - CPU moved 30->12%
//
// It is plain ASCII on purpose: piped through a PowerShell 5.1 console, an
// em dash or an arrow comes out as mojibake.
func (r Result) String() string {
	if reason, void := r.Void(); void {
		return fmt.Sprintf("%s %s: VOID - %s", r.Service, r.Action, reason)
	}
	return fmt.Sprintf("%s %s: %s mW (%s -> %s mW, CPU %s)",
		r.Service, r.Action, signed(r.DeltaMW()),
		commas(r.Before.AvgMW), commas(r.After.AvgMW), cpuRange(r.CPUBefore, r.CPUAfter))
}

// Observer is told what Run has done so far. A run takes the better part of
// half a minute, so a caller wants to show each side as it lands; and if the
// run is interrupted, the caller must know whether the service has already
// been touched, because "interrupted" is a lie once the machine has changed.
type Observer interface {
	// Baseline is called with the first measurement, before the service is
	// touched.
	Baseline(m battery.Measurement, cpu float64)

	// Transitioned is called once the service reports its new state and
	// before the settle wait, with how long the transition took.
	Transitioned(took time.Duration)
}

// Controller is whatever can stop and start a service for the caller: the
// control package itself where the process is elevated, the helper's client
// where it is not. Measuring never needs rights; only the act does.
type Controller interface {
	Stop(name string) error
	Start(name string) error
}

// Run measures, acts through ctl, settles, and measures again, telling obs
// as it goes.
//
// It refuses to act on AC: there is no discharge to read, so nothing would be
// learned, and whether to stop the service anyway is the caller's decision,
// not this package's. Errors from ctl are returned as they are. Run does not
// apply the confounder rule itself; it reports both sides in full and leaves
// the verdict to Result.String, so the caller always has the numbers to show.
func Run(ctx context.Context, service string, action Action, obs Observer, ctl Controller) (Result, error) {
	s, err := battery.Read()
	if err != nil {
		return Result{}, err
	}
	if s.AcOnline {
		return Result{}, battery.ErrOnAC
	}

	r := Result{Service: service, Action: action}
	r.Before, r.CPUBefore, err = Sample(ctx)
	if err != nil {
		return Result{}, err
	}
	obs.Baseline(r.Before, r.CPUBefore)

	started := time.Now()
	switch action {
	case Stop:
		err = ctl.Stop(service)
	case Start:
		err = ctl.Start(service)
	default:
		err = fmt.Errorf("unknown action %d", int(action))
	}
	if err != nil {
		return Result{}, err
	}
	obs.Transitioned(time.Since(started))

	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-time.After(SettleTime):
	}

	r.After, r.CPUAfter, err = Sample(ctx)
	if err != nil {
		return Result{}, err
	}
	return r, nil
}

// Sample takes one battery measurement and returns it with the CPU load over
// the same window, which is the confounder every comparison is judged
// against. Each side of a Run is one Sample; so is the measure command.
func Sample(ctx context.Context) (battery.Measurement, float64, error) {
	before, err := cpuload.Take()
	if err != nil {
		return battery.Measurement{}, 0, err
	}
	m, err := battery.Measure(ctx, battery.DefaultSamples, battery.DefaultInterval)
	if err != nil {
		return battery.Measurement{}, 0, err
	}
	after, err := cpuload.Take()
	if err != nil {
		return battery.Measurement{}, 0, err
	}
	return m, cpuload.Percent(before, after), nil
}

// signed formats a delta with its sign always shown, so a saving reads as a
// saving at a glance.
func signed(mw int) string {
	if mw < 0 {
		return "-" + commas(-mw)
	}
	return "+" + commas(mw)
}

func cpuRange(before, after float64) string {
	return fmt.Sprintf("%.0f->%.0f%%", before, after)
}

// commas writes a non-negative integer with thousands separators.
func commas(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	head := len(s) % 3
	b.WriteString(s[:head])
	for i := head; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
