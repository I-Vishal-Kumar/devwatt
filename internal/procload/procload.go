// Package procload attributes CPU time to processes, so the tray can say who
// is busy and not only how busy the machine is.
//
// This is the attribution the triage method allows, and no more. Watts come
// from the battery controller; nothing per-process reports watts, and every
// "power usage" column that claims to is a model. CPU share is the one
// per-process number that is cheap to read and honest, and on battery a
// process holding a core for half a minute is the thing worth naming. Like
// cpuload it is a difference between two snapshots; a snapshot on its own
// says nothing.
package procload

import (
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/I-Vishal-Kumar/devwatt/internal/cpuload"
)

// Snapshot is the cumulative CPU time of every process that could be asked,
// at one instant.
type Snapshot struct {
	taken time.Time
	procs map[uint32]proc
}

type proc struct {
	name string // executable file name, lower-cased so casing never splits one program in two
	cpu  uint64 // kernel + user, in 100 ns ticks
}

// Take reads every process's CPU time. It works unelevated: a process that
// refuses PROCESS_QUERY_LIMITED_INFORMATION (a protected one) is skipped,
// the same filtering discover applies to protected services, and its time
// still shows in the whole-machine number cpuload reports.
func Take() (Snapshot, error) {
	// The documented contract: the snapshot can fail with ERROR_BAD_LENGTH
	// while the process list is changing under it, and the caller retries
	// until it succeeds. Bounded here so a machine that never settles is an
	// error rather than a hang.
	var snap windows.Handle
	var err error
	for attempt := 0; attempt < 8; attempt++ {
		snap, err = windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
		if !errors.Is(err, windows.ERROR_BAD_LENGTH) {
			break
		}
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)

	s := Snapshot{taken: time.Now(), procs: make(map[uint32]proc)}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == 0 {
			continue
		}
		h, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID)
		if openErr != nil {
			continue
		}
		var creation, exit, kernel, user windows.Filetime
		timesErr := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user)
		windows.CloseHandle(h)
		if timesErr != nil {
			continue // gone between the snapshot and the query
		}
		s.procs[e.ProcessID] = proc{
			name: strings.ToLower(windows.UTF16ToString(e.ExeFile[:])),
			cpu:  cpuload.Ticks(kernel) + cpuload.Ticks(user),
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return Snapshot{}, fmt.Errorf("Process32Next: %w", err)
	}
	return s, nil
}

// Proc is one program's share of the machine between two snapshots, with
// every process of that name folded together: a browser is its renderers.
type Proc struct {
	Name    string  `json:"name"`
	Percent float64 `json:"percent"` // share of all logical processors, 0-100
	Count   int     `json:"count"`   // processes of this name
}

// Top ranks programs by CPU share between prev and cur and returns the first
// n. Only a process present in both snapshots, under the same name, counts:
// one that started or exited in between has no interval to measure, and a
// reused pid is a different program.
func Top(prev, cur Snapshot, n int) []Proc {
	wall := uint64(cur.taken.Sub(prev.taken) / 100) // 100 ns ticks
	if wall == 0 {
		return nil
	}

	type share struct {
		busy  uint64
		count int
	}
	byName := make(map[string]*share)
	for pid, c := range cur.procs {
		p, ok := prev.procs[pid]
		if !ok || p.name != c.name || c.cpu < p.cpu {
			continue
		}
		s := byName[c.name]
		if s == nil {
			s = &share{}
			byName[c.name] = s
		}
		s.busy += c.cpu - p.cpu
		s.count++
	}

	capacity := float64(wall * uint64(runtime.NumCPU()))
	out := make([]Proc, 0, len(byName))
	for name, s := range byName {
		out = append(out, Proc{Name: name, Percent: float64(s.busy) * 100 / capacity, Count: s.count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Percent != out[j].Percent {
			return out[i].Percent > out[j].Percent
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
