// Package cpuload measures whole-machine CPU busy time between two instants.
//
// devwatt records it alongside every battery measurement as a confounder. A
// discharge delta means nothing on its own: if CPU load moved more than the
// effect being measured, the test is void and must be reported as void, not
// as a result. A Windows Update worker exiting between two samples once
// turned a brightness test negative — "raising brightness saved power" —
// because its ~2 W departure swamped the effect. Recording load on both sides
// is what catches that.
package cpuload

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Snapshot is the cumulative idle and busy time of every logical processor
// combined, in 100 ns units, at one instant.
type Snapshot struct {
	idle  uint64
	total uint64
}

// Take reads the system-wide counters. It works unelevated.
func Take() (Snapshot, error) {
	var idle, kernel, user windows.Filetime
	r, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return Snapshot{}, fmt.Errorf("GetSystemTimes: %w", err)
	}
	// Kernel time includes idle time, so kernel+user is the whole clock.
	return Snapshot{
		idle:  Ticks(idle),
		total: Ticks(kernel) + Ticks(user),
	}, nil
}

// Percent is the share of CPU time spent busy between two snapshots, 0–100.
// It returns 0 when no time has passed.
func Percent(from, to Snapshot) float64 {
	return percent(to.idle-from.idle, to.total-from.total)
}

func percent(idle, total uint64) float64 {
	if total == 0 || idle > total {
		return 0
	}
	return float64(total-idle) * 100 / float64(total)
}

// Ticks is a FILETIME as the 100 ns count it is. Filetime.Nanoseconds is
// not usable here: it subtracts the 1601 epoch, which is right for a
// timestamp and wrong for an elapsed time.
func Ticks(ft windows.Filetime) uint64 {
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

var (
	kernel32           = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes = kernel32.NewProc("GetSystemTimes")
)
