// Package catalog defines the allowlist of services devwatt is willing to
// show and control.
//
// The rule is deliberately strict: a service is managed by default only if
// it matches an entry here; anything else needs the user to adopt it by
// hand. We never control arbitrary services or processes, because stopping
// something whose dependencies we don't understand is how a "power tool"
// turns into a broken machine. Adding to this list is a deliberate act, not
// a discovery heuristic.
package catalog

import "regexp"

// Entry describes one class of developer-owned service that is safe to stop
// and start by hand.
type Entry struct {
	Display string
	Note    string

	pattern *regexp.Regexp
}

// matches reports whether a Windows service name belongs to this entry.
func (e Entry) matches(serviceName string) bool {
	return e.pattern.MatchString(serviceName)
}

// entries is the allowlist. Patterns are anchored and case-insensitive.
//
// Keep these narrow. A pattern like `.*sql.*` would sweep in system
// components; the versioned suffixes below are what real installers produce
// (for example "postgresql-x64-17", "MSSQL$SQLEXPRESS").
var entries = []Entry{
	{
		Display: "PostgreSQL",
		Note:    "Local database server. Safe to stop when no project needs it.",
		pattern: regexp.MustCompile(`(?i)^postgresql([-_](x64|x86))?([-_]?\d+(\.\d+)?)?$`),
	},
	{
		Display: "MongoDB",
		Note:    "Local database server. Also requests a 1ms platform timer, which blocks deep CPU idle states.",
		pattern: regexp.MustCompile(`(?i)^mongodb(\d+(\.\d+)?)?$`),
	},
	{
		Display: "MySQL",
		Note:    "Local database server. Safe to stop when no project needs it.",
		pattern: regexp.MustCompile(`(?i)^mysql\d*$`),
	},
	{
		Display: "MariaDB",
		Note:    "Local database server. Safe to stop when no project needs it.",
		pattern: regexp.MustCompile(`(?i)^mariadb\d*$`),
	},
	{
		Display: "Redis",
		Note:    "In-memory cache. Data is volatile; stopping clears it.",
		pattern: regexp.MustCompile(`(?i)^redis$`),
	},
	{
		Display: "SQL Server",
		Note:    "Local SQL Server instance.",
		pattern: regexp.MustCompile(`(?i)^(MSSQLSERVER|MSSQL\$[\w-]+)$`),
	},
	{
		Display: "Elasticsearch",
		Note:    "Search server. Heavy JVM; usually a large idle cost.",
		pattern: regexp.MustCompile(`(?i)^elasticsearch([-_]service[-_]x64)?$`),
	},
	{
		Display: "RabbitMQ",
		Note:    "Message broker. Queued messages persist across restarts.",
		pattern: regexp.MustCompile(`(?i)^rabbitmq$`),
	},
	{
		Display: "Docker Desktop",
		Note:    "Stopping this stops every running container.",
		pattern: regexp.MustCompile(`(?i)^com\.docker\.service$`),
	},
	{
		Display: "nginx",
		Note:    "Local web server.",
		pattern: regexp.MustCompile(`(?i)^nginx$`),
	},
	{
		Display: "Jenkins",
		Note:    "Local CI server. Heavy JVM.",
		pattern: regexp.MustCompile(`(?i)^jenkins$`),
	},
	{
		Display: "Ollama",
		Note:    "Local model runtime. Can hold GPU memory even when idle.",
		pattern: regexp.MustCompile(`(?i)^ollama$`),
	},
}

// denied names are never controllable, by any tier, ever. This is defence in
// depth: the catalog decides what ships enabled, discovery decides what may be
// suggested, and this gate sits under both so that neither a widened pattern
// nor a user tick can reach these.
//
// Discovery on a real machine surfaced WinDefend, WdNisSvc, MDCoreSvc and a VPN
// client as "safe-looking" candidates — they run outside C:\Windows, aren't
// svchost-hosted, and have no dependent services. Every structural heuristic we
// have says they are fine to stop. They are not. Hence this list.
var denied = map[string]bool{
	// Core OS — stopping these breaks the machine, or breaks devwatt itself.
	"winmgmt":          true, // WMI — half the OS management stack breaks without it
	"rpcss":            true,
	"dcomlaunch":       true,
	"lsass":            true,
	"power":            true,
	"plugplay":         true,
	"eventlog":         true,
	"schedule":         true,
	"nsi":              true,
	"dhcp":             true,
	"dnscache":         true,
	"audiosrv":         true,
	"cryptsvc":         true,
	"bfe":              true,
	"mpssvc":           true,
	"gpsvc":            true,
	"profsvc":          true,
	"themes":           true,
	"userdatasvc":      true,
	"trustedinstaller": true,
	"wuauserv":         true,
}

// deniedPatterns block whole categories that must never be controllable
// regardless of name. Security software is the important one: disabling it to
// save a watt is a trade nobody should be offered by a battery tool.
var deniedPatterns = []*regexp.Regexp{
	// Defender and endpoint protection.
	regexp.MustCompile(`(?i)^(windefend|wdnissvc|wscsvc|sense|mdcoresvc|securityhealthservice)$`),
	// Any third-party AV/EDR that follows the usual naming.
	regexp.MustCompile(`(?i)(antivirus|endpoint|crowdstrike|sentinelone|sophos|mcafee|symantec|eset|kaspersky|bitdefender)`),
	// VPN and network filtering — stopping these can silently drop protection.
	regexp.MustCompile(`(?i)(vpn|warp|zscaler|netskope|tailscale|wireguard)`),
	// Elevation brokers: stopping them breaks updaters in confusing ways.
	regexp.MustCompile(`(?i)elevationservice$`),
	// Disk encryption.
	regexp.MustCompile(`(?i)^(bdesvc|bitlocker)`),
}

// Denied reports whether a service is hard-blocked. Callers must consult this
// before showing or controlling anything, including entries a user has
// explicitly added to their own config.
func Denied(serviceName string) bool {
	if denied[lower(serviceName)] {
		return true
	}
	for _, p := range deniedPatterns {
		if p.MatchString(serviceName) {
			return true
		}
	}
	return false
}

// Lookup returns the catalog entry matching a Windows service name.
//
// ok is false when the service is not in the shipped catalog. That is not the
// same as "must not be shown": a service the user has adopted through discovery
// is legitimate but has no catalog entry. Callers decide using both this and
// Denied. Only Denied is an absolute veto.
func Lookup(serviceName string) (Entry, bool) {
	if Denied(serviceName) {
		return Entry{}, false
	}
	for _, e := range entries {
		if e.matches(serviceName) {
			return e, true
		}
	}
	return Entry{}, false
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
