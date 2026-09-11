// Package discover finds services that are plausibly developer-owned, so that
// devwatt works on a machine whose services nobody hardcoded.
//
// Discovery only ever *suggests*. It narrows the field — on a real Windows 11
// laptop, 294 services reduce to about 33 — but the survivors still included
// Windows Defender, its network inspection service, and a VPN client. Every
// structural signal available (installed outside C:\Windows, own process, no
// dependent services) said those were safe to stop. They were not.
//
// So the rule is: catalog entries may be enabled automatically, discovered
// candidates may not. A discovered service becomes controllable only after the
// user explicitly adopts it, and catalog.Denied vetoes both paths.
package discover

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/I-Vishal-Kumar/devwatt/internal/catalog"
	"github.com/I-Vishal-Kumar/devwatt/internal/scm"
)

// Candidate is a service devwatt is willing to tell the user about.
type Candidate struct {
	Name       string
	Display    string
	BinaryPath string
	Running    bool
	StartType  string

	// Entry and InCatalog are set when the shipped catalog recognises this
	// service. Those are safe to enable without asking; everything else needs
	// the user to opt in.
	Entry     catalog.Entry
	InCatalog bool
}

// List returns catalog matches and discovery candidates found on this machine.
//
// It needs only read access to the service database, so it runs unelevated —
// deliberately, so `devwatt list` works from any terminal and nothing about
// looking requires the rights that acting does.
func List() ([]Candidate, error) {
	// The OS-component heuristic in classify is a path prefix; without the
	// real prefix it would either keep everything or match nothing, so refuse
	// rather than guess.
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return nil, errors.New("SystemRoot is not set")
	}
	systemRoot = strings.ToLower(systemRoot)

	m, err := scm.OpenManager(windows.SC_MANAGER_CONNECT | windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, err
	}
	defer m.Disconnect()

	names, err := m.ListServices()
	if err != nil {
		return nil, err
	}

	var out []Candidate
	for _, name := range names {
		// Absolute veto first, so a denied service is never even inspected.
		if catalog.Denied(name) {
			continue
		}

		s, err := scm.OpenService(m, name,
			windows.SERVICE_QUERY_CONFIG|windows.SERVICE_QUERY_STATUS|windows.SERVICE_ENUMERATE_DEPENDENTS)
		if err != nil {
			continue // typically access denied on a protected service; skip quietly
		}

		c, keep := classify(s, name, systemRoot)
		if keep {
			out = append(out, c)
		}
		s.Close()
	}
	return out, nil
}

// classify decides whether one service is worth showing, and how.
func classify(s *mgr.Service, name, systemRoot string) (Candidate, bool) {
	cfg, err := s.Config()
	if err != nil {
		return Candidate{}, false
	}

	bin := binaryOf(cfg.BinaryPathName)

	// A catalog match is trusted on name alone. Installers put databases in
	// varied locations and we would rather recognise "postgresql-x64-17"
	// wherever it lives than lose it to a path heuristic.
	entry, inCatalog := catalog.Lookup(name)

	if !inCatalog {
		// Heuristics, in the order that removes the most for the least work.
		if strings.HasPrefix(strings.ToLower(bin), systemRoot) {
			return Candidate{}, false // OS component
		}
		if strings.Contains(strings.ToLower(filepath.Base(bin)), "svchost") {
			return Candidate{}, false // shared host process, never ours to stop
		}
		if hasDependents(s) {
			return Candidate{}, false // something else needs it
		}
	}

	st, err := s.Query()
	if err != nil {
		return Candidate{}, false
	}

	return Candidate{
		Name:       name,
		Display:    cfg.DisplayName,
		BinaryPath: bin,
		Running:    st.State == svc.Running,
		StartType:  startTypeName(cfg.StartType),
		Entry:      entry,
		InCatalog:  inCatalog,
	}, true
}

// hasDependents reports whether any other service depends on this one.
// We ask for a zero-length buffer: Windows answers ERROR_MORE_DATA when there
// is something to report, which is all we need to know.
func hasDependents(s *mgr.Service) bool {
	var bytesNeeded, returned uint32
	r, _, _ := procEnumDependentServices.Call(
		uintptr(s.Handle),
		uintptr(serviceStateAll),
		0,
		0,
		uintptr(unsafe.Pointer(&bytesNeeded)),
		uintptr(unsafe.Pointer(&returned)),
	)
	if r != 0 {
		return returned > 0 // succeeded outright: dependents only if counted
	}
	return bytesNeeded > 0
}

// binaryOf extracts the executable from a service's command line, which may be
// quoted and may carry arguments.
func binaryOf(pathName string) string {
	p := strings.TrimSpace(pathName)
	if p == "" {
		return ""
	}
	if p[0] == '"' {
		if end := strings.Index(p[1:], `"`); end >= 0 {
			return p[1 : end+1]
		}
		return strings.Trim(p, `"`)
	}
	// Unquoted: arguments start at the first " -" or " /".
	for _, sep := range []string{" -", " /"} {
		if i := strings.Index(p, sep); i > 0 {
			p = p[:i]
		}
	}
	return strings.TrimSpace(p)
}

func startTypeName(t uint32) string {
	switch t {
	case mgr.StartAutomatic:
		return "Automatic"
	case mgr.StartManual:
		return "Manual"
	case mgr.StartDisabled:
		return "Disabled"
	default:
		return "Unknown"
	}
}

const serviceStateAll = 3

var (
	advapi32                  = windows.NewLazySystemDLL("advapi32.dll")
	procEnumDependentServices = advapi32.NewProc("EnumDependentServicesW")
)
