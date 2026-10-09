package obs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// monitorsDir holds the checked-in monitor definitions.
const monitorsDir = "../../deploy/datadog/monitors"

type monitorDef struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Query   string   `json:"query"`
	Message string   `json:"message"`
	Tags    []string `json:"tags"`
	Options struct {
		Thresholds map[string]float64 `json:"thresholds"`
	} `json:"options"`
}

func loadMonitors(t *testing.T) map[string]monitorDef {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(monitorsDir, "*.json"))
	if err != nil {
		t.Fatalf("globbing %s: %v", monitorsDir, err)
	}
	if len(paths) == 0 {
		t.Fatalf("no monitors in %s", monitorsDir)
	}

	out := make(map[string]monitorDef, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		var m monitorDef
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("%s is not valid JSON: %v", p, err)
		}
		out[filepath.Base(p)] = m
	}
	return out
}

// A monitor on a metric nothing emits never fires, which is the worst failure
// mode an alert has: it looks like health. Same argument as the dashboard's
// equivalent test, and more serious, because nobody opens a monitor to check
// whether it is working.
func TestMonitorsOnlyAlertOnEmittedMetrics(t *testing.T) {
	emitted := map[string]bool{}
	for _, name := range MetricNames() {
		emitted[name] = true
	}

	var unknown []string
	for file, m := range loadMonitors(t) {
		found := metricRef.FindAllStringSubmatch(m.Query, -1)
		if len(found) == 0 {
			t.Errorf("%s: no metric found in the query %q", file, m.Query)
			continue
		}
		for _, ref := range found {
			if !emitted[ref[1]] {
				unknown = append(unknown, file+": "+ref[1])
			}
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("monitors alert on metrics this code never emits: %v\n"+
			"an alert that cannot fire is indistinguishable from a healthy system", unknown)
	}
}

// The fields the API requires, plus the ones that make an alert useful to the
// person it wakes up.
func TestMonitorsAreCompleteAndActionable(t *testing.T) {
	for file, m := range loadMonitors(t) {
		if m.Name == "" {
			t.Errorf("%s has no name", file)
		}
		if m.Type != "query alert" {
			t.Errorf("%s: type = %q, want \"query alert\"", file, m.Type)
		}
		if m.Query == "" {
			t.Errorf("%s has no query", file)
		}
		if len(m.Options.Thresholds) == 0 {
			t.Errorf("%s declares no thresholds, so the query's own comparison is "+
				"all there is and the alert cannot be tuned without editing it", file)
		}

		// A notification that says only "MOS is low" costs the reader the same
		// investigation every time. Each message here has to name something
		// concrete to look at next — a metric, the call log, or the trace —
		// rather than restating the threshold it just crossed.
		if len(m.Message) < 200 {
			t.Errorf("%s: the message is %d characters; it should say what to check next",
				file, len(m.Message))
		}
		named := false
		for _, where := range []string{"voice.", "call log", "trace"} {
			if strings.Contains(m.Message, where) {
				named = true
			}
		}
		if !named {
			t.Errorf("%s: the message names nothing to look at next", file)
		}

		// Tags are what let a monitor be found and routed. service and env at
		// minimum, since everything this project emits carries both.
		have := map[string]bool{}
		for _, tag := range m.Tags {
			if k, _, ok := strings.Cut(tag, ":"); ok {
				have[k] = true
			}
		}
		for _, want := range []string{"service", "env"} {
			if !have[want] {
				t.Errorf("%s has no %s tag (tags: %v)", file, want, m.Tags)
			}
		}
	}
}

// The monitors have to cover both halves of the demo's argument. One that only
// watched the network would miss a sick provider entirely, and the whole point
// of the provider-degraded beat is that the two are distinguishable.
func TestMonitorsCoverBothLayers(t *testing.T) {
	monitors := loadMonitors(t)

	var network, ai bool
	for _, m := range monitors {
		switch {
		case strings.Contains(m.Query, "voice.rtp."), strings.Contains(m.Query, "voice.mos."):
			network = true
		case strings.Contains(m.Query, "voice.llm."), strings.Contains(m.Query, "voice.stt."),
			strings.Contains(m.Query, "voice.call."), strings.Contains(m.Query, "voice.tts."):
			ai = true
		}
	}
	if !network {
		t.Error("no monitor watches the transport layer")
	}
	if !ai {
		t.Error("no monitor watches the AI pipeline")
	}
	if len(monitors) < 3 {
		t.Errorf("%d monitors is too few to cover network, AI latency and outright failures",
			len(monitors))
	}
}

// The demo's beats are built to trip specific monitors, so at least one has to
// have a threshold the scripted impairment actually crosses. lossy-wan loses
// about 5% of packets and conceals a similar share, which lands MOS near 2;
// provider-degraded adds three seconds per LLM round trip.
func TestMonitorThresholdsMatchTheScriptedImpairment(t *testing.T) {
	monitors := loadMonitors(t)

	check := func(file string, crossed func(float64) bool, why string) {
		m, ok := monitors[file]
		if !ok {
			t.Errorf("%s is missing", file)
			return
		}
		crit, ok := m.Options.Thresholds["critical"]
		if !ok {
			t.Errorf("%s has no critical threshold", file)
			return
		}
		if !crossed(crit) {
			t.Errorf("%s: critical threshold %g will not be crossed by the demo (%s)",
				file, crit, why)
		}
	}

	// lossy-wan drives MOS to roughly 2.1, so a threshold above that fires.
	check("call-quality.json", func(v float64) bool { return v > 2.2 },
		"lossy-wan measures MOS near 2.1")
	// lossy-wan conceals roughly 6-7% of audio, so a critical threshold has to
	// sit below that to fire, and a warning below it to fire earlier.
	check("concealed-audio.json", func(v float64) bool { return v < 15 },
		"lossy-wan conceals roughly 6-7% of audio")
	// provider-degraded adds 3s per LLM round trip, and a tool-calling turn
	// makes two, so turns land near 6s.
	check("provider-latency.json", func(v float64) bool { return v < 6000 },
		"provider-degraded makes a tool-calling turn take about 6s")
}

// markerValue pulls the number out of a Datadog marker, which is written as a
// line equation: "y = 3", "y > 4000".
var markerValue = regexp.MustCompile(`y\s*[=<>]+\s*(-?[0-9.]+)`)

// TestDashboardMarkersMatchMonitorThresholds keeps the two halves of the story
// honest with each other.
//
// The dashboard draws threshold lines so a beat is unmistakable on screen, and
// the monitors alert on thresholds of their own. If the two drift apart, a
// graph shows a call sitting comfortably under its line while an alert fires
// about it — and in a demo that discrepancy is the only thing anyone will
// remember. Every line drawn has to be a threshold something actually alerts
// on.
func TestDashboardMarkersMatchMonitorThresholds(t *testing.T) {
	thresholds := map[float64]bool{}
	for _, m := range loadMonitors(t) {
		for _, v := range m.Options.Thresholds {
			thresholds[v] = true
		}
	}

	data, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatalf("reading %s: %v", dashboardPath, err)
	}

	var dash struct {
		Widgets []struct {
			Definition struct {
				Title   string `json:"title"`
				Markers []struct {
					Value string `json:"value"`
					Label string `json:"label"`
				} `json:"markers"`
			} `json:"definition"`
		} `json:"widgets"`
	}
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatal(err)
	}

	var drawn int
	for _, w := range dash.Widgets {
		for _, mk := range w.Definition.Markers {
			match := markerValue.FindStringSubmatch(mk.Value)
			if match == nil {
				t.Errorf("%q: marker %q has no numeric value", w.Definition.Title, mk.Value)
				continue
			}
			v, err := strconv.ParseFloat(match[1], 64)
			if err != nil {
				t.Errorf("%q: marker %q: %v", w.Definition.Title, mk.Value, err)
				continue
			}
			drawn++
			if !thresholds[v] {
				t.Errorf("%q draws a line at %g, which no monitor alerts on; "+
					"a graph and an alert disagreeing is worse than neither",
					w.Definition.Title, v)
			}
			// The label names the monitor, so a viewer knows the line is an
			// alert threshold rather than someone's opinion.
			if !strings.Contains(strings.ToLower(mk.Label), "monitor") {
				t.Errorf("%q: marker %q is labelled %q, which does not say it is "+
					"a monitor threshold", w.Definition.Title, mk.Value, mk.Label)
			}
		}
	}

	if drawn == 0 {
		t.Error("the dashboard draws no threshold lines, so nothing on it is " +
			"visibly good or bad without reading the numbers")
	}
	t.Logf("%d threshold lines, all matching monitor thresholds", drawn)
}
