// Package alert decides what the tray should tell the user, and when.
//
// It is pure. It takes what the tray already knows on every tick and returns
// the notices to show now, so every threshold, cooldown and once-per-session
// rule lives in one place with no Win32 in it, and the tray stays a thin
// shell that reads, evaluates and shows. Everything here applies only on
// battery: on AC there is no draw to judge and nothing to save.
package alert

import (
	"fmt"
	"strings"
	"time"

	"github.com/I-Vishal-Kumar/devwatt/internal/battery"
	"github.com/I-Vishal-Kumar/devwatt/internal/config"
	"github.com/I-Vishal-Kumar/devwatt/internal/procload"
)

// Service is one managed service as the tray sees it.
type Service struct {
	Name, Display string
	Running       bool
	Cost          *config.Cost // last A/B result, nil when never measured
}

// Input is one tick's worth of what the tray knows.
type Input struct {
	Now        time.Time
	OnBattery  bool
	Window     battery.Measurement
	WindowFull bool
	CPU        float64
	Top        []procload.Proc
	Managed    []Service
	Cfg        config.Config
}

// Notice is one notification to show.
type Notice struct {
	Title, Text string
}

// cooldown is the least time between two notices with the same key. Ten
// minutes is long enough that a machine that stays hot does not nag, and
// short enough that a new cause after the old one went away is still named.
const cooldown = 10 * time.Minute

// Rules is the bookkeeping the rules need between ticks. The zero value is
// ready to use.
type Rules struct {
	onBattery     bool            // as of the last tick; a change starts or ends a session
	lastEval      time.Time       // when the last on-battery tick was, for streaks
	summarised    bool            // the session's summary has been shown
	baselineAsked bool            // the session's "set a baseline" nudge has been shown
	warned        map[string]bool // managed services already named this session
	streak        map[string]time.Duration
	lastFired     map[string]time.Time // per notice key, kept across sessions
}

// Evaluate runs every rule against one tick and returns what to show.
func (r *Rules) Evaluate(in Input) []Notice {
	if !in.OnBattery {
		// Leaving battery ends the session, so the next unplug starts clean:
		// a new summary, every service warnable again, no streak carried
		// over from work done on AC. Cooldowns are not per-session; a plug
		// and unplug inside ten minutes should not re-fire everything.
		r.onBattery = false
		r.summarised = false
		r.baselineAsked = false
		r.warned = nil
		r.streak = nil
		return nil
	}
	var elapsed time.Duration
	if r.onBattery {
		elapsed = in.Now.Sub(r.lastEval)
	}
	r.onBattery, r.lastEval = true, in.Now
	if r.warned == nil {
		r.warned = make(map[string]bool)
		r.streak = make(map[string]time.Duration)
	}
	if r.lastFired == nil {
		r.lastFired = make(map[string]time.Time)
	}

	steady := in.WindowFull && in.Window.Trustworthy()
	var out []Notice

	// Summary: once per session, at the first number that can be believed,
	// so the user knows what "on battery" costs before anything else fires.
	if !r.summarised && steady {
		r.summarised = true
		out = append(out, Notice{
			Title: "On battery",
			Text:  fmt.Sprintf("%s, swing %s - %.1f h left", battery.Watts(in.Window.AvgMW), battery.Watts(in.Window.SwingMW), in.Window.Runtime().Hours()),
		})
	}

	// No baseline yet: ask for one, once per session, at the first number
	// that could be it. The baseline is never set automatically — a seed
	// taken during a hog would silently disable the high-draw rule for good,
	// and only the user knows whether the machine is idle right now.
	if in.Cfg.BaselineMW == 0 && steady && !r.baselineAsked && r.ready("baseline", in.Now) {
		r.baselineAsked = true
		out = append(out, Notice{
			Title: "Set a baseline",
			Text: fmt.Sprintf("Draw is %s now. If the machine is idle, open the dashboard and click Set current so devwatt can tell you when draw runs high.",
				battery.Watts(in.Window.AvgMW)),
		})
	}

	// High draw: the draw is well above the machine's own baseline, and the
	// top CPU users are named because they are the likeliest reason.
	if base := in.Cfg.BaselineMW; base > 0 && steady && in.Window.AvgMW >= base*(100+in.Cfg.HighDrawPercent)/100 && r.ready("high", in.Now) {
		text := fmt.Sprintf("%s is %d%% above your %s baseline - %.1f h left",
			battery.Watts(in.Window.AvgMW), (in.Window.AvgMW-base)*100/base, battery.Watts(base), in.Window.Runtime().Hours())
		if top := topCPU(in.Top); top != "" {
			text += ". Top CPU: " + top
		}
		out = append(out, Notice{Title: "High power draw", Text: text})
	}

	// Hog: one program has held a large share of the machine for a sustained
	// stretch. A burst is normal; half a minute is something to name.
	above := make(map[string]bool, len(in.Top))
	for _, p := range in.Top {
		if p.Percent < float64(in.Cfg.HogCPUPercent) {
			continue
		}
		above[p.Name] = true
		r.streak[p.Name] += elapsed
		sustain := time.Duration(in.Cfg.HogSustainSeconds) * time.Second
		if r.streak[p.Name] >= sustain && r.ready("hog:"+p.Name, in.Now) {
			text := fmt.Sprintf("%.0f%% CPU for %.0f s on battery (%s)", p.Percent, r.streak[p.Name].Seconds(), processes(p.Count))
			if in.WindowFull {
				text += " - draw " + battery.Watts(in.Window.AvgMW)
			}
			out = append(out, Notice{Title: p.Name + " is busy", Text: text})
		}
	}
	for name := range r.streak {
		if !above[name] {
			delete(r.streak, name)
		}
	}

	// Managed service running on battery: once per session per service, with
	// what it was last measured to cost. Skipped entirely when the policy is
	// on, because the policy has already stopped it.
	if !in.Cfg.StopOnBattery {
		for _, s := range in.Managed {
			if !s.Running || r.warned[s.Name] {
				continue
			}
			r.warned[s.Name] = true
			out = append(out, Notice{Title: s.Display + " is running on battery", Text: costText(s.Cost)})
		}
	}
	return out
}

// ready reports whether a notice with this key may fire now, and records
// that it did.
func (r *Rules) ready(key string, now time.Time) bool {
	if last, ok := r.lastFired[key]; ok && now.Sub(last) < cooldown {
		return false
	}
	r.lastFired[key] = now
	return true
}

// topCPU names the two biggest CPU users worth naming: below 5% a program
// is not why the draw is high.
func topCPU(top []procload.Proc) string {
	var parts []string
	for _, p := range top {
		if p.Percent < 5 || len(parts) == 2 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s %.0f%%", p.Name, p.Percent))
	}
	return strings.Join(parts, ", ")
}

func costText(c *config.Cost) string {
	switch {
	case c == nil:
		return "Not measured yet - stop it from the tray to price it."
	case c.Void != "":
		return fmt.Sprintf("Last measurement was void (%s). Stop it from the tray.", c.Void)
	}
	// DeltaMW is after minus before; a stop that saved 900 mW and a start
	// that added 900 mW are the same 0.9 W cost.
	cost := c.DeltaMW
	if c.Action == "stopped" {
		cost = -cost
	}
	return fmt.Sprintf("Last measured cost %s. Stop it from the tray.", battery.Watts(cost))
}

func processes(n int) string {
	if n == 1 {
		return "1 process"
	}
	return fmt.Sprintf("%d processes", n)
}
