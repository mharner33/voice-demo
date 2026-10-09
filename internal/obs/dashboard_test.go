package obs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// dashboardPath is the checked-in dashboard definition.
const dashboardPath = "../../deploy/datadog/dashboard.json"

// metricRef matches a metric name inside a Datadog query, which looks like
// "p95:voice.rtp.jitter_ms{...}" or "sum:voice.call.turns{...}.as_count()".
var metricRef = regexp.MustCompile(`\b(?:avg|sum|min|max|count|p50|p75|p90|p95|p99):([a-z0-9_.]+)\{`)

// TestDashboardOnlyUsesEmittedMetrics is what makes the dashboard trustworthy.
// A dashboard that charts a metric the code never emits is worse than no
// dashboard: it shows an empty graph during a demo and the viewer cannot tell
// whether the system is healthy or the query is wrong.
func TestDashboardOnlyUsesEmittedMetrics(t *testing.T) {
	data, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatalf("reading %s: %v", dashboardPath, err)
	}

	emitted := map[string]bool{}
	for _, name := range MetricNames() {
		emitted[name] = true
	}

	found := map[string]bool{}
	for _, m := range metricRef.FindAllStringSubmatch(string(data), -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatal("no metric queries were found in the dashboard; the regex or the file is wrong")
	}

	var unknown []string
	for name := range found {
		// Metrics Datadog derives itself (ml_obs.*) are not emitted by this
		// code, so they are legitimately absent from MetricNames.
		if strings.HasPrefix(name, "ml_obs.") {
			continue
		}
		if !emitted[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("the dashboard charts metrics this code never emits: %v\n"+
			"either emit them or fix the dashboard; an empty graph in a demo is "+
			"indistinguishable from a healthy system", unknown)
	}

	t.Logf("dashboard references %d distinct metrics, all emitted", len(found))
}

// TestDashboardIsValidAndComplete checks the structure Datadog requires and
// that the dashboard actually covers all three layers, which is its whole
// purpose.
func TestDashboardIsValidAndComplete(t *testing.T) {
	data, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatalf("reading %s: %v", dashboardPath, err)
	}

	var dash struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		LayoutType  string `json:"layout_type"`
		Widgets     []struct {
			Definition struct {
				Type  string `json:"type"`
				Title string `json:"title"`
			} `json:"definition"`
		} `json:"widgets"`
	}
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatalf("%s is not valid JSON: %v", dashboardPath, err)
	}

	if dash.Title == "" {
		t.Error("the dashboard has no title")
	}
	if dash.LayoutType != "ordered" && dash.LayoutType != "free" {
		t.Errorf("layout_type = %q, want ordered or free", dash.LayoutType)
	}
	if len(dash.Widgets) < 5 {
		t.Errorf("the dashboard has %d widgets, which is too few to tell the story",
			len(dash.Widgets))
	}
	for i, w := range dash.Widgets {
		if w.Definition.Type == "" {
			t.Errorf("widget %d has no type", i)
		}
		if w.Definition.Type != "note" && w.Definition.Title == "" {
			t.Errorf("widget %d (%s) has no title", i, w.Definition.Type)
		}
	}

	// All three layers must be represented, since the point of the dashboard is
	// showing the connection between them.
	body := string(data)
	for layer, probe := range map[string]string{
		"network":       "voice.rtp.",
		"jitter buffer": "voice.jbuf.",
		"AI pipeline":   "voice.call.",
	} {
		if !strings.Contains(body, probe) {
			t.Errorf("the dashboard charts nothing from the %s layer (%s)", layer, probe)
		}
	}
}

// TestDashboardTemplateVariablesAreUsed guards against a template variable that
// silently filters nothing.
func TestDashboardTemplateVariablesAreUsed(t *testing.T) {
	data, err := os.ReadFile(dashboardPath)
	if err != nil {
		t.Fatal(err)
	}

	var dash struct {
		TemplateVariables []struct {
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"template_variables"`
	}
	if err := json.Unmarshal(data, &dash); err != nil {
		t.Fatal(err)
	}

	body := string(data)
	for _, tv := range dash.TemplateVariables {
		if !strings.Contains(body, "$"+tv.Name) {
			t.Errorf("template variable %q is declared but never used in a query", tv.Name)
		}
		// The prefix has to be a tag the code actually attaches, or the
		// variable's dropdown will be empty.
		if tv.Prefix == "" {
			continue
		}
		switch tv.Prefix {
		case "env", "service", "version":
			// Unified service tags, applied to every metric.
		default:
			tagged := false
			// Every field set, so the check is against the tags this code
			// can emit rather than the ones a past version happened to.
			for _, tag := range (CallTags{
				Codec: "x", ProviderProfile: "x", NetworkProfile: "x",
				STTProvider: "x", LLMProvider: "x", TTSProvider: "x",
			}).Slice() {
				if strings.HasPrefix(tag, tv.Prefix+":") {
					tagged = true
				}
			}
			if !tagged {
				t.Errorf("template variable prefix %q is not a tag this code emits; "+
					"its dropdown would be empty", tv.Prefix)
			}
		}
	}
}

// TestDashboardFileLives keeps the path honest if the deploy layout moves.
func TestDashboardFileLives(t *testing.T) {
	if _, err := os.Stat(filepath.Clean(dashboardPath)); err != nil {
		t.Fatalf("the dashboard is not at %s: %v", dashboardPath, err)
	}
}
