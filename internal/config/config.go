// Package config persists what the user decides and what devwatt has
// learned: which discovered services are adopted into the managed set,
// whether devwatt may stop that set on its own on battery, the thresholds
// the alerts fire at, the machine's baseline draw, and the last measured
// cost of each service.
//
// Nothing here changes what the catalog ships enabled; adoption only widens
// the managed set with services the user has looked at and ticked. And the
// widening has a floor: every adopted name is checked against catalog.Denied
// on load, so editing the file by hand cannot reach a denied service either.
// That is the "user tick" half of the promise catalog.go makes. The same
// goes for the rest of the file: what it holds is validated on load, so the
// code that reads it later can trust it instead of guarding every use.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/I-Vishal-Kumar/devwatt/internal/catalog"
)

// Config is what the file holds.
type Config struct {
	Adopted []string `json:"adopted"`

	// StopOnBattery is the one policy: when the charger comes out, stop
	// every managed service that is running, and start those same ones
	// again when it comes back.
	StopOnBattery bool `json:"stop_on_battery"`

	// BaselineMW is the machine's sustained draw with nothing extra running,
	// which "high draw" is judged against; 0 until it has been measured.
	BaselineMW int `json:"baseline_mw"`

	// Alert thresholds. HighDrawPercent is how far above the baseline the
	// draw must be; HogCPUPercent and HogSustainSeconds are how much of the
	// machine one program must hold, and for how long, before it is named.
	HighDrawPercent   int `json:"high_draw_percent"`
	HogCPUPercent     int `json:"hog_cpu_percent"`
	HogSustainSeconds int `json:"hog_sustain_seconds"`

	// Costs is the last A/B result per service name, so a running service
	// can be reported with what it was last measured to cost.
	Costs map[string]Cost `json:"costs"`

	// Notifications is the switch for toasts; every alert is still logged
	// in the dashboard. MaxToastsPerDay caps how many toasts a day may
	// interrupt: the rules' cooldowns are the first line against a flood
	// and this is the second.
	Notifications   bool `json:"notifications"`
	MaxToastsPerDay int  `json:"max_toasts_per_day"`

	// Theme is the dashboard's appearance: one of Themes. "system" follows
	// the OS preference.
	Theme string `json:"theme"`
}

// Themes are the values Theme may hold.
var Themes = []string{"system", "light", "dark"}

// Cost is one remembered A/B result. DeltaMW is after minus before, as cost
// reports it; Action says which way the service went, because the same
// delta means a saving after a stop and a cost after a start. The CPU pair
// is kept because a delta without its confounder is not a result.
type Cost struct {
	Action    string    `json:"action"` // "stopped" or "started"
	DeltaMW   int       `json:"delta_mw"`
	BeforeMW  int       `json:"before_mw"`
	AfterMW   int       `json:"after_mw"`
	CPUBefore float64   `json:"cpu_before"`
	CPUAfter  float64   `json:"cpu_after"`
	Void      string    `json:"void"` // why the measurement did not count, or "" when it did
	When      time.Time `json:"when"`
}

// defaults is the config before the user has changed anything: thresholds
// set, nothing adopted, nothing measured.
func defaults() Config {
	return Config{
		HighDrawPercent:   30,
		HogCPUPercent:     25,
		HogSustainSeconds: 30,
		Costs:             map[string]Cost{},
		Notifications:     true,
		MaxToastsPerDay:   20,
		Theme:             "system",
	}
}

// Load reads the config, or returns defaults when there is no file yet: a
// first run is a state, not a failure. An existing file is decoded over
// defaults, so a field it does not mention keeps its default — a file from
// before notifications had a switch keeps them on, while an explicit false
// is respected; that is JSON's partial decode, and what lets an older file
// work after a field is added. Anything wrong with what the file does say
// is an error naming it.
func Load() (Config, error) {
	p, err := path()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return defaults(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", p, err)
	}
	c := defaults()
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", p, err)
	}
	for _, name := range c.Adopted {
		if catalog.Denied(name) {
			return Config{}, fmt.Errorf("config: adopted service %q is on the denied list", name)
		}
	}
	for _, t := range []struct {
		field string
		value int
	}{
		{"high_draw_percent", c.HighDrawPercent},
		{"hog_cpu_percent", c.HogCPUPercent},
		{"hog_sustain_seconds", c.HogSustainSeconds},
		{"max_toasts_per_day", c.MaxToastsPerDay},
	} {
		if t.value <= 0 {
			return Config{}, fmt.Errorf("config: %s must be greater than 0 in %s", t.field, p)
		}
	}
	if c.BaselineMW < 0 {
		return Config{}, fmt.Errorf("config: baseline_mw must not be negative in %s", p)
	}
	if c.Costs == nil {
		return Config{}, fmt.Errorf("config: costs must be an object in %s", p)
	}
	if !slices.Contains(Themes, c.Theme) {
		return Config{}, fmt.Errorf("config: theme must be one of %s in %s", strings.Join(Themes, ", "), p)
	}
	for name, cost := range c.Costs {
		if cost.Action != "stopped" && cost.Action != "started" {
			return Config{}, fmt.Errorf("config: costs[%q].action must be \"stopped\" or \"started\" in %s", name, p)
		}
	}
	return c, nil
}

// Save writes the config, creating its directory on first use.
func Save(c Config) error {
	p, err := path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return fmt.Errorf("config: write %s: %w", p, err)
	}
	return nil
}

// IsAdopted reports whether the user has adopted a service by name.
func (c Config) IsAdopted(name string) bool {
	return slices.Contains(c.Adopted, name)
}

// SetAdopted adopts or un-adopts a service. The list stays sorted and free
// of duplicates, so the file is stable across saves and diffs cleanly.
func (c *Config) SetAdopted(name string, on bool) {
	c.Adopted = slices.DeleteFunc(c.Adopted, func(s string) bool { return s == name })
	if on {
		c.Adopted = append(c.Adopted, name)
		slices.Sort(c.Adopted)
	}
}

// path is %APPDATA%\devwatt\config.json. APPDATA missing is an error, not a
// cue to guess a directory.
func path() (string, error) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		return "", errors.New("APPDATA is not set")
	}
	return filepath.Join(appData, "devwatt", "config.json"), nil
}
