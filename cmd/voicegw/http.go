package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/mharner33/voice-demo/internal/chaos"
	"github.com/mharner33/voice-demo/internal/faults"
)

// The gateway's small HTTP surface exists for one reason: a demo has to be
// able to change conditions while a dashboard is already on screen. Restarting
// the gateway between beats would break every live graph, clear the call list,
// and take the audience's attention with it.
//
// Only *provider* impairment is settable here. Network impairment belongs to
// the client and cannot be moved: nothing in an RTP stream says how it was
// degraded, so the gateway has no way to apply or even observe it (plan
// finding 24). The responses say so rather than leaving the asymmetry to be
// discovered.
const networkNote = "network impairment is applied by the client (voicectl -profile), " +
	"not here: the gateway cannot degrade a stream it only receives"

// providerProfile is the currently applied provider-impairment profile.
//
// It is mutable because /chaos changes it, and it has to be read wherever the
// profile is used as a metric tag: a call impaired as provider-degraded must
// not be tagged with the profile the gateway happened to start with, or the
// dashboard would attribute a beat's latency to the previous beat.
type providerProfile struct {
	v atomic.Value // string
}

func newProviderProfile(name string) *providerProfile {
	p := &providerProfile{}
	p.Set(name)
	return p
}

func (p *providerProfile) Get() string {
	s, _ := p.v.Load().(string)
	return s
}

func (p *providerProfile) Set(name string) { p.v.Store(name) }

// chaosState is the JSON shape of the provider impairment, in both directions.
// Pointers, so that a POST can set one knob without implying zeros for the
// rest: `{"llm_latency_ms": 3000}` must leave the STT error rate alone.
type chaosRequest struct {
	// Profile names a whole scenario. Applied first, so explicit knobs below
	// refine it rather than being overwritten by it.
	Profile *string `json:"profile,omitempty"`

	STTLatencyMs *float64 `json:"stt_latency_ms,omitempty"`
	STTErrorRate *float64 `json:"stt_error_rate,omitempty"`
	LLMLatencyMs *float64 `json:"llm_latency_ms,omitempty"`
	LLMErrorRate *float64 `json:"llm_error_rate,omitempty"`
	TTSLatencyMs *float64 `json:"tts_latency_ms,omitempty"`
}

// stageState reports one provider's impairment and what it has actually
// injected, which is the difference between "configured to fail 10%" and
// "failed 10% of the calls that happened".
type stageState struct {
	LatencyMs float64 `json:"latency_ms"`
	ErrorRate float64 `json:"error_rate"`

	Calls  uint64 `json:"calls"`
	Errors uint64 `json:"errors"`
}

type chaosState struct {
	Profile string `json:"provider_profile"`
	STT     stageState
	LLM     stageState
	TTS     stageState
	Note    string `json:"note"`
}

// MarshalJSON names the stage fields, which struct tags on embedded values
// cannot do as legibly.
func (s chaosState) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"provider_profile": s.Profile,
		"stt":              s.STT,
		"llm":              s.LLM,
		"tts":              s.TTS,
		"note":             s.Note,
	})
}

// chaosAPI serves the live impairment endpoint.
type chaosAPI struct {
	faults  providerFaults
	profile *providerProfile

	// liveCalls reports how many calls are in progress, for /healthz.
	liveCalls func() int
}

func (a *chaosAPI) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/chaos", a.handleChaos)
	mux.HandleFunc("/healthz", a.handleHealth)
	return mux
}

func (a *chaosAPI) state() chaosState {
	stage := func(inj *faults.Injector) stageState {
		cfg := inj.Config()
		st := inj.Stats()
		return stageState{
			LatencyMs: float64(cfg.ExtraLatency.Milliseconds()),
			ErrorRate: cfg.ErrorRate,
			Calls:     st.Calls,
			Errors:    st.Errors,
		}
	}
	return chaosState{
		Profile: a.profile.Get(),
		STT:     stage(a.faults.stt),
		LLM:     stage(a.faults.llm),
		TTS:     stage(a.faults.tts),
		Note:    networkNote,
	}
}

func (a *chaosAPI) handleChaos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, a.state())

	case http.MethodPost, http.MethodPut:
		var req chaosRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("parsing the request body: %w", err))
			return
		}
		if err := a.apply(req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, a.state())

	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed,
			fmt.Errorf("%s is not allowed on /chaos", r.Method))
	}
}

// apply retunes the injectors.
//
// The whole request is validated before anything is changed. A partially
// applied request would leave the gateway in a state no profile describes,
// which during a demo is the worst possible outcome: the dashboard would be
// showing conditions nobody can name.
func (a *chaosAPI) apply(req chaosRequest) error {
	pv := chaos.Provider{
		STTExtraLatencyMs: float64(a.faults.stt.Config().ExtraLatency.Milliseconds()),
		STTErrorRate:      a.faults.stt.Config().ErrorRate,
		LLMExtraLatencyMs: float64(a.faults.llm.Config().ExtraLatency.Milliseconds()),
		LLMErrorRate:      a.faults.llm.Config().ErrorRate,
		TTSExtraLatencyMs: float64(a.faults.tts.Config().ExtraLatency.Milliseconds()),
	}
	name := a.profile.Get()

	if req.Profile != nil {
		prof, err := chaos.LookupProfile(*req.Profile)
		if err != nil {
			return err
		}
		// A named profile replaces the provider settings wholesale, including
		// clearing the ones it does not set. Anything else would make `clean`
		// fail to clean up after the previous beat.
		pv = prof.Provider
		name = prof.Name
	}

	for _, o := range []struct {
		val *float64
		dst *float64
	}{
		{req.STTLatencyMs, &pv.STTExtraLatencyMs},
		{req.STTErrorRate, &pv.STTErrorRate},
		{req.LLMLatencyMs, &pv.LLMExtraLatencyMs},
		{req.LLMErrorRate, &pv.LLMErrorRate},
		{req.TTSLatencyMs, &pv.TTSExtraLatencyMs},
	} {
		if o.val != nil {
			*o.dst = *o.val
			// A knob set by hand no longer matches any named profile, and
			// saying "mobile" when the numbers are something else would
			// mislabel every metric the beat produces.
			if req.Profile == nil {
				name = "custom"
			}
		}
	}

	if err := pv.Validate(); err != nil {
		return err
	}

	ms := func(v float64) time.Duration { return time.Duration(v) * time.Millisecond }
	settings := []struct {
		inj *faults.Injector
		cfg faults.Config
	}{
		{a.faults.stt, faults.Config{
			ExtraLatency: ms(pv.STTExtraLatencyMs), ErrorRate: pv.STTErrorRate, Tunable: true}},
		{a.faults.llm, faults.Config{
			ExtraLatency: ms(pv.LLMExtraLatencyMs), ErrorRate: pv.LLMErrorRate, Tunable: true}},
		{a.faults.tts, faults.Config{
			ExtraLatency: ms(pv.TTSExtraLatencyMs), Tunable: true}},
	}
	// Validated before any of it is applied, for the reason above.
	for _, s := range settings {
		if err := s.cfg.Validate(); err != nil {
			return err
		}
	}
	for _, s := range settings {
		if err := s.inj.SetConfig(s.cfg); err != nil {
			return err
		}
	}

	a.profile.Set(name)
	return nil
}

func (a *chaosAPI) handleHealth(w http.ResponseWriter, r *http.Request) {
	live := 0
	if a.liveCalls != nil {
		live = a.liveCalls()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"live_calls":       live,
		"provider_profile": a.profile.Get(),
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
