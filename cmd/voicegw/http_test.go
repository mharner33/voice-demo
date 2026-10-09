package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mharner33/voice-demo/internal/faults"
	"github.com/mharner33/voice-demo/internal/stt"
)

// newTestAPI builds the endpoint over three tunable injectors, exactly as the
// gateway does.
func newTestAPI(t *testing.T, startProfile string) *chaosAPI {
	t.Helper()

	inj := func() *faults.Injector {
		i, err := faults.New(faults.Config{Tunable: true})
		if err != nil {
			t.Fatalf("faults.New: %v", err)
		}
		return i
	}
	transcriber, err := stt.NewMock(stt.MockConfig{})
	if err != nil {
		t.Fatalf("stt.NewMock: %v", err)
	}

	return &chaosAPI{
		faults: providerFaults{
			stt: inj(), llm: inj(), tts: inj(),
			sttStream: stt.WithStreamFault(transcriber),
		},
		profile:   newProviderProfile(startProfile),
		liveCalls: func() int { return 3 },
	}
}

// post sends a /chaos request and returns the decoded response.
func post(t *testing.T, api *chaosAPI, body string) (int, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/chaos", strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return rec.Code, out
}

func get(t *testing.T, api *chaosAPI, path string) (int, map[string]any) {
	t.Helper()

	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return rec.Code, out
}

func stage(t *testing.T, resp map[string]any, name string) map[string]any {
	t.Helper()
	s, ok := resp[name].(map[string]any)
	if !ok {
		t.Fatalf("response has no %q stage: %v", name, resp)
	}
	return s
}

// Applying a named profile is the demo's main move: one call changes a beat.
func TestChaosAppliesANamedProfile(t *testing.T) {
	api := newTestAPI(t, "clean")

	code, resp := post(t, api, `{"profile":"provider-degraded"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, resp)
	}

	if got := resp["provider_profile"]; got != "provider-degraded" {
		t.Errorf("profile = %v, want provider-degraded", got)
	}
	if got := stage(t, resp, "stt")["latency_ms"]; got != 2000.0 {
		t.Errorf("stt latency = %v, want 2000", got)
	}
	if got := stage(t, resp, "stt")["error_rate"]; got != 0.1 {
		t.Errorf("stt error rate = %v, want 0.1", got)
	}
	if got := stage(t, resp, "llm")["latency_ms"]; got != 3000.0 {
		t.Errorf("llm latency = %v, want 3000", got)
	}

	// And the injectors themselves, not just the report: the response could
	// echo the request back without anything having changed.
	if got := api.faults.stt.Config().ExtraLatency; got != 2*time.Second {
		t.Errorf("the STT injector waits %v, want 2s", got)
	}
	if got := api.faults.llm.Config().ExtraLatency; got != 3*time.Second {
		t.Errorf("the LLM injector waits %v, want 3s", got)
	}
}

// The profile tag has to follow the endpoint, or a beat's metrics would be
// attributed to the profile that was in effect before it — which is the one
// mistake that would make the whole dashboard story wrong.
func TestChaosUpdatesTheProfileTag(t *testing.T) {
	api := newTestAPI(t, "clean")

	if _, _ = post(t, api, `{"profile":"provider-degraded"}`); api.profile.Get() != "provider-degraded" {
		t.Errorf("the profile tag is %q after applying provider-degraded", api.profile.Get())
	}
	if _, _ = post(t, api, `{"profile":"clean"}`); api.profile.Get() != "clean" {
		t.Errorf("the profile tag is %q after applying clean", api.profile.Get())
	}
}

// Going back to clean must actually clear the previous beat, not merge with
// it: a demo that could not return to a baseline would only work once.
func TestChaosCleanClearsPreviousImpairment(t *testing.T) {
	api := newTestAPI(t, "clean")

	post(t, api, `{"profile":"provider-degraded"}`)
	code, resp := post(t, api, `{"profile":"clean"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, resp)
	}

	for _, name := range []string{"stt", "llm", "tts"} {
		s := stage(t, resp, name)
		if s["latency_ms"] != 0.0 || s["error_rate"] != 0.0 {
			t.Errorf("%s is still impaired after clean: %v", name, s)
		}
	}
	if api.faults.llm.Config().ExtraLatency != 0 {
		t.Errorf("the LLM injector still waits %v", api.faults.llm.Config().ExtraLatency)
	}
}

// One knob at a time, without implying zeros for the others: a POST that set
// only the LLM latency must not quietly cure a sick recognizer.
func TestChaosSetsOneKnobWithoutClearingTheRest(t *testing.T) {
	api := newTestAPI(t, "clean")

	post(t, api, `{"stt_latency_ms":1500,"stt_error_rate":0.25}`)
	code, resp := post(t, api, `{"llm_latency_ms":500}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, resp)
	}

	if got := stage(t, resp, "stt")["latency_ms"]; got != 1500.0 {
		t.Errorf("stt latency = %v after setting only the LLM knob, want 1500", got)
	}
	if got := stage(t, resp, "stt")["error_rate"]; got != 0.25 {
		t.Errorf("stt error rate = %v, want 0.25", got)
	}
	if got := stage(t, resp, "llm")["latency_ms"]; got != 500.0 {
		t.Errorf("llm latency = %v, want 500", got)
	}
}

// Hand-set knobs no longer match any named profile, and the tag has to say so
// rather than claiming a name whose numbers it no longer has.
func TestChaosHandTunedBecomesCustom(t *testing.T) {
	api := newTestAPI(t, "clean")

	post(t, api, `{"stt_latency_ms":750}`)
	if got := api.profile.Get(); got != "custom" {
		t.Errorf("profile = %q after a hand-set knob, want custom", got)
	}

	// A profile plus a refinement keeps the profile's name, since the beat is
	// still that profile — just adjusted.
	post(t, api, `{"profile":"mobile","tts_latency_ms":100}`)
	if got := api.profile.Get(); got != "mobile" {
		t.Errorf("profile = %q after mobile plus a refinement, want mobile", got)
	}
	if got := api.faults.tts.Config().ExtraLatency; got != 100*time.Millisecond {
		t.Errorf("tts latency = %v, want the refinement to have applied", got)
	}
}

// A bad request changes nothing. A partially applied retune would leave the
// gateway in a state no profile describes, with a dashboard on screen showing
// conditions nobody can name.
func TestChaosRejectsBadRequestsAtomically(t *testing.T) {
	api := newTestAPI(t, "clean")
	post(t, api, `{"profile":"provider-degraded"}`)

	before := api.faults.stt.Config()

	for _, body := range []string{
		`{"profile":"not-a-profile"}`,
		`{"stt_error_rate":1.5}`,
		`{"llm_latency_ms":-10}`,
		`not json at all`,
	} {
		code, resp := post(t, api, body)
		if code != http.StatusBadRequest {
			t.Errorf("POST %s returned %d, want 400", body, code)
		}
		if _, ok := resp["error"]; !ok {
			t.Errorf("POST %s returned no error message: %v", body, resp)
		}
	}

	if got := api.faults.stt.Config(); got != before {
		t.Errorf("a rejected request changed the configuration: %+v -> %+v", before, got)
	}
	if got := api.profile.Get(); got != "provider-degraded" {
		t.Errorf("a rejected request changed the profile tag to %q", got)
	}
}

// GET reports both the configuration and what has actually been injected,
// which is the difference between "set to fail one call in ten" and "failed
// one call in ten".
func TestChaosGetReportsInjectedCounts(t *testing.T) {
	api := newTestAPI(t, "clean")
	post(t, api, `{"llm_error_rate":1}`)

	// Four calls through the injector, all of which should fail.
	for i := 0; i < 4; i++ {
		if err := api.faults.llm.Apply(t.Context()); err == nil {
			t.Fatalf("call %d was not failed by a 100%% error rate", i)
		}
	}

	code, resp := get(t, api, "/chaos")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	llm := stage(t, resp, "llm")
	if llm["calls"] != 4.0 {
		t.Errorf("calls = %v, want 4", llm["calls"])
	}
	if llm["errors"] != 4.0 {
		t.Errorf("errors = %v, want 4", llm["errors"])
	}
}

// The response says that the network side is not settable here. The asymmetry
// is real — the gateway cannot degrade a stream it only receives — and leaving
// it to be discovered would cost someone a confusing ten minutes mid-demo.
func TestChaosExplainsThatTheNetworkIsClientSide(t *testing.T) {
	api := newTestAPI(t, "clean")

	_, resp := get(t, api, "/chaos")
	note, ok := resp["note"].(string)
	if !ok || !strings.Contains(note, "client") {
		t.Errorf("note = %v, want it to say the network side belongs to the client", resp["note"])
	}
}

func TestChaosRejectsOtherMethods(t *testing.T) {
	api := newTestAPI(t, "clean")

	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/chaos", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /chaos returned %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got == "" {
		t.Error("a 405 response has no Allow header")
	}
}

func TestHealthz(t *testing.T) {
	api := newTestAPI(t, "mobile")

	code, resp := get(t, api, "/healthz")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if resp["status"] != "ok" {
		t.Errorf("status = %v, want ok", resp["status"])
	}
	// Live call count and the current profile, so a demo script can wait for
	// the gateway and then see which beat it is in.
	if resp["live_calls"] != 3.0 {
		t.Errorf("live_calls = %v, want 3", resp["live_calls"])
	}
	if resp["provider_profile"] != "mobile" {
		t.Errorf("provider_profile = %v, want mobile", resp["provider_profile"])
	}
}

// The mid-call recognizer failure is armable over the endpoint, because it is
// a demo beat like any other — and it is the beat an audience asks for.
func TestChaosArmsTheMidCallRecognizerFailure(t *testing.T) {
	api := newTestAPI(t, "clean")

	code, resp := post(t, api, `{"profile":"stt-dropout"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %v", code, resp)
	}
	if got := resp["provider_profile"]; got != "stt-dropout" {
		t.Errorf("profile = %v, want stt-dropout", got)
	}
	if got := stage(t, resp, "stt")["fail_after_ms"]; got != 3000.0 {
		t.Errorf("fail_after_ms = %v, want 3000", got)
	}
	if got := api.faults.sttStream.FailAfter(); got != 3*time.Second {
		t.Errorf("the wrapper is armed for %v, want 3s", got)
	}

	// And it is cleared by going back to a profile that does not set it, like
	// every other knob: a beat that could not be turned off would poison the
	// rest of the session.
	post(t, api, `{"profile":"clean"}`)
	if got := api.faults.sttStream.FailAfter(); got != 0 {
		t.Errorf("still armed for %v after clean", got)
	}
}

// A rejected request must not leave the recognizer set to die mid-call, which
// is why arming happens last.
func TestChaosRejectedRequestDoesNotArmTheDropout(t *testing.T) {
	api := newTestAPI(t, "clean")

	code, _ := post(t, api, `{"stt_fail_after_ms":2000,"llm_error_rate":9}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", code)
	}
	if got := api.faults.sttStream.FailAfter(); got != 0 {
		t.Errorf("a rejected request armed the dropout for %v", got)
	}
}
