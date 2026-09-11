// Package battery reads real power draw from the battery controller.
//
// The controller reports watts. Everything else — Task Manager's "Power usage"
// column, SRUM, any per-process estimate — is a model built on top of it.
// devwatt measures the total from hardware first and only then tries to
// attribute it, never the other way round.
//
// Two constraints shape this package. The number only exists while
// discharging: on AC there is nothing to read, so callers get ErrOnAC rather
// than a meaningless zero. And a single reading is worthless: draw swings by
// hundreds of milliwatts on its own and spikes far higher during bursts, so a
// sample caught mid-burst reads 27 W on a machine that sustains 17 W. Measure
// samples repeatedly and reports the average together with its swing, and the
// swing is what says whether the average can be believed.
package battery

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// State is one reading from the composite battery, as the ACPI driver
// reports it. It is the same data WMI's BatteryStatus exposes, read directly
// so devwatt needs neither COM nor the WMI service to get a number.
type State struct {
	AcOnline bool
	Present  bool

	// RateMW is signed as the driver reports it: negative while discharging,
	// positive while charging, zero when neither.
	RateMW int32

	// RemainingMWh and FullMWh are the current charge and the last full
	// charge. FullMWh is what the battery holds today, not its design
	// capacity, so RemainingMWh/FullMWh is the percentage the OS shows.
	RemainingMWh uint32
	FullMWh      uint32
}

// DischargeMW is the discharge rate as a positive number, or 0 when the
// battery is not discharging.
func (s State) DischargeMW() int {
	if s.RateMW < 0 {
		return int(-s.RateMW)
	}
	return 0
}

// Discharging reports whether this reading is a discharge reading: unplugged
// and drawing. On the tick the charger goes in the controller reports a rate
// of 0 before AcOnline flips, and that 0 is not a reading of anything; a
// caller that averages or charts discharge must take only these.
func (s State) Discharging() bool {
	return !s.AcOnline && s.RateMW < 0
}

// Percent is the charge level as the OS shows it.
func (s State) Percent() int {
	return percent(s.RemainingMWh, s.FullMWh)
}

var (
	// ErrOnAC means there is no discharge to read. Report it as such; never
	// report the zero the controller would otherwise hand back.
	ErrOnAC = errors.New("on AC power: the battery controller reports no discharge while plugged in")

	// ErrNoDischarge means the machine is unplugged but the controller has
	// not yet reported a draw — the tick before AcOnline catches up with the
	// charger, typically. A measurement must not carry that zero.
	ErrNoDischarge = errors.New("unplugged but the controller reports no discharge; try again in a moment")

	// errNoBattery means the machine has no battery to read.
	errNoBattery = errors.New("no battery present")
)

// Read returns the current battery state. It works unelevated.
func Read() (State, error) {
	var raw systemBatteryState
	status, _, _ := procCallNtPowerInformation.Call(
		systemBatteryStateLevel,
		0, 0,
		uintptr(unsafe.Pointer(&raw)), unsafe.Sizeof(raw),
	)
	// The return is an NTSTATUS in the low 32 bits; anything non-zero failed.
	if code := uint32(status); code != 0 {
		return State{}, fmt.Errorf("CallNtPowerInformation(SystemBatteryState): NTSTATUS 0x%08x", code)
	}

	s := State{
		AcOnline:     raw.AcOnLine != 0,
		Present:      raw.BatteryPresent != 0,
		RateMW:       raw.Rate,
		RemainingMWh: raw.RemainingCapacity,
		FullMWh:      raw.MaxCapacity,
	}
	if !s.Present {
		return s, errNoBattery
	}
	return s, nil
}

// Sampling defaults. Eight readings 1.2 s apart is enough to average out the
// controller's own jitter without making the user wait; anything shorter
// keeps catching bursts. The ACPI driver refreshes on its own cadence (about
// every 2.5 s on the machine this was built on), so consecutive readings often
// repeat; that is the driver, not a stuck sample, and the average is unharmed.
const (
	DefaultSamples  = 8
	DefaultInterval = 1200 * time.Millisecond

	// TrustworthySwingMW is the spread below which a measurement's readings
	// are stable enough for its average to be believed. Wider means something
	// is bursting, and the caller should find it before drawing conclusions
	// from the number.
	TrustworthySwingMW = 600
)

// Measurement is a sampled discharge reading: the sustained average, how much
// the readings moved, and the charge left to run it down.
type Measurement struct {
	// ReadingsMW are the individual discharge readings in the order taken.
	// Keep them: when a first reading looks alarming and later ones settle,
	// the user should see that rather than have it quietly dropped.
	ReadingsMW []int

	AvgMW   int
	MinMW   int
	MaxMW   int
	SwingMW int

	// RemainingMWh and FullMWh are taken from the final reading.
	RemainingMWh uint32
	FullMWh      uint32
}

// Trustworthy reports whether the readings were stable enough to act on.
func (m Measurement) Trustworthy() bool {
	return m.SwingMW < TrustworthySwingMW
}

// Runtime estimates how long the remaining charge lasts at this measurement's
// average, or 0 when the average is zero. It is a projection from twenty
// seconds of readings and is labelled "at this draw" wherever it is shown;
// the tray's "time left" comes from minutes of readings instead.
func (m Measurement) Runtime() time.Duration {
	if m.AvgMW <= 0 {
		return 0
	}
	hours := float64(m.RemainingMWh) / float64(m.AvgMW)
	return time.Duration(hours * float64(time.Hour))
}

// Percent is the charge level at the end of the measurement.
func (m Measurement) Percent() int {
	return percent(m.RemainingMWh, m.FullMWh)
}

// Measure takes samples readings, interval apart, and summarises them.
//
// The first reading is taken immediately. If the machine is on AC at any
// point — including being plugged in mid-run — the measurement is void and
// ErrOnAC is returned, because a partial average with a zero in it is worse
// than no number; a reading that is unplugged yet shows no draw voids it
// the same way, as ErrNoDischarge. Cancelling ctx returns ctx.Err().
func Measure(ctx context.Context, samples int, interval time.Duration) (Measurement, error) {
	readings := make([]int, 0, samples)
	var last State
	for i := 0; i < samples; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return Measurement{}, ctx.Err()
			case <-time.After(interval):
			}
		}
		s, err := Read()
		if err != nil {
			return Measurement{}, err
		}
		if s.AcOnline {
			return Measurement{}, ErrOnAC
		}
		if !s.Discharging() {
			return Measurement{}, ErrNoDischarge
		}
		readings = append(readings, s.DischargeMW())
		last = s
	}

	m := summarise(readings)
	m.RemainingMWh = last.RemainingMWh
	m.FullMWh = last.FullMWh
	return m, nil
}

// summarise computes the statistics for a set of readings.
func summarise(readings []int) Measurement {
	m := Measurement{ReadingsMW: readings}
	if len(readings) == 0 {
		return m
	}
	m.MinMW, m.MaxMW = readings[0], readings[0]
	sum := 0
	for _, r := range readings {
		sum += r
		if r < m.MinMW {
			m.MinMW = r
		}
		if r > m.MaxMW {
			m.MaxMW = r
		}
	}
	// Round half up, as a person would.
	m.AvgMW = (sum + len(readings)/2) / len(readings)
	m.SwingMW = m.MaxMW - m.MinMW
	return m
}

// Watts formats a milliwatt figure the way every line meant for a person
// shows it: to a tenth of a watt, which is all the controller's jitter
// leaves meaningful.
func Watts(mw int) string {
	return fmt.Sprintf("%.1f W", float64(mw)/1000)
}

func percent(remaining, full uint32) int {
	if full == 0 {
		return 0
	}
	return int((uint64(remaining)*100 + uint64(full)/2) / uint64(full))
}

// systemBatteryState mirrors SYSTEM_BATTERY_STATE from powerbase.h. Field
// order and widths matter; the layout is what the kernel writes into.
type systemBatteryState struct {
	AcOnLine          uint8
	BatteryPresent    uint8
	Charging          uint8
	Discharging       uint8
	Spare1            [3]uint8
	Tag               uint8
	MaxCapacity       uint32 // mWh
	RemainingCapacity uint32 // mWh
	Rate              int32  // mW; negative while discharging
	// EstimatedTime is the driver's own time-left guess. On the machine this
	// was built on it is exactly RemainingCapacity/Rate at the instant, which
	// jumps with every burst; the tray projects from minutes of its own
	// readings instead, so the field is only here to keep the layout.
	EstimatedTime uint32
	DefaultAlert1 uint32
	DefaultAlert2 uint32
}

// POWER_INFORMATION_LEVEL value for SystemBatteryState.
const systemBatteryStateLevel = 5

var (
	powrprof                   = windows.NewLazySystemDLL("powrprof.dll")
	procCallNtPowerInformation = powrprof.NewProc("CallNtPowerInformation")
)

// Window is a rolling view of the last DefaultSamples discharge readings, for
// a caller that polls the battery anyway and wants the same sustained
// average and swing Measure reports, without stopping to take a run.
//
// Only readings taken while discharging belong in it. The caller Resets it
// when the machine is plugged in: readings from either side of an AC
// transition are two different machines, and a zero from the charger is not
// a reading at all.
type Window struct {
	readings [DefaultSamples]int
	n        int // readings held, up to DefaultSamples
	next     int // slot the next reading overwrites
	last     State
}

// Add records one reading, dropping the oldest once the window is full.
func (w *Window) Add(s State) {
	w.readings[w.next] = s.DischargeMW()
	w.next = (w.next + 1) % DefaultSamples
	if w.n < DefaultSamples {
		w.n++
	}
	w.last = s
}

// Reset forgets every reading.
func (w *Window) Reset() {
	*w = Window{}
}

// Len is how many readings the window holds, up to DefaultSamples.
func (w *Window) Len() int {
	return w.n
}

// Summary is the readings in the order they were taken, summarised the way
// Measure summarises a run, with the charge from the latest reading.
func (w *Window) Summary() Measurement {
	oldest := 0
	if w.n == DefaultSamples {
		oldest = w.next
	}
	readings := make([]int, 0, w.n)
	for i := 0; i < w.n; i++ {
		readings = append(readings, w.readings[(oldest+i)%DefaultSamples])
	}
	m := summarise(readings)
	m.RemainingMWh = w.last.RemainingMWh
	m.FullMWh = w.last.FullMWh
	return m
}
