package obs

import (
	"fmt"
	"sort"

	"github.com/DataDog/dd-trace-go/v2/llmobs"
)

// Evaluation labels. They are constants because an evaluation is only useful
// when the same label is submitted on every call — a typo produces a second,
// nearly-empty series in the Evaluations view rather than an error.
const (
	// EvalTranscriptConfidence is the recognizer's mean confidence across the
	// call. It is the evaluation that matters most for this demo: it is an AI
	// quality figure that moves when the *network* degrades, which is the whole
	// argument for instrumenting the transport and the model together.
	EvalTranscriptConfidence = "transcript_confidence"

	// EvalAudioQuality buckets the call by how much of the audio the jitter
	// buffer had to invent. Categorical rather than numeric so the Evaluations
	// view can group calls by it.
	EvalAudioQuality = "audio_quality"

	// EvalReplied records whether every turn produced something to say. A turn
	// that yields no reply is silence on the line, which is a failure the
	// transport metrics cannot see.
	EvalReplied = "replied_every_turn"
)

// Audio quality buckets for EvalAudioQuality.
const (
	AudioQualityClean    = "clean"
	AudioQualityDegraded = "degraded"
	AudioQualityPoor     = "poor"
)

// Concealment thresholds separating the audio quality buckets. The boundaries
// are the points where mock recognizer confidence starts to fall measurably,
// which keeps the bucket label and the confidence score telling the same story.
const (
	audioDegradedAbove = 0.02
	audioPoorAbove     = 0.10
)

// AudioQualityBucket classifies a call by its concealed fraction.
func AudioQualityBucket(concealedFraction float64) string {
	switch {
	case concealedFraction > audioPoorAbove:
		return AudioQualityPoor
	case concealedFraction > audioDegradedAbove:
		return AudioQualityDegraded
	default:
		return AudioQualityClean
	}
}

// evalTags renders a tag map in the SDK's "k:v" form. The keys are sorted so
// that two calls with the same tags produce byte-identical tag lists, which
// makes the submissions comparable in a diff and in a test.
func evalTags(tags map[string]string) []string {
	if len(tags) == 0 {
		return nil
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s:%s", k, tags[k]))
	}
	return out
}

// evalOpts builds the common option set for a submission.
func (t *Tracer) evalOpts(tags map[string]string) []llmobs.EvaluationOption {
	var opts []llmobs.EvaluationOption
	if ts := evalTags(tags); len(ts) > 0 {
		opts = append(opts, llmobs.WithEvaluationTags(ts))
	}
	return opts
}

// canEvaluate reports whether a submission would reach anywhere. An evaluation
// is addressed by span and trace ID, so a span that was never created has
// nothing to attach to.
func (t *Tracer) canEvaluate(s *Span) bool {
	return t.LLMObsEnabled() && s != nil && s.obs != nil
}

// EvalScore submits a numeric evaluation against a span.
//
// The span may already be finished: a submission is addressed by span and trace
// ID rather than by holding the span open. It must still happen before the
// tracer is stopped, since that is what flushes the submission.
func (t *Tracer) EvalScore(s *Span, label string, value float64, tags map[string]string) {
	if !t.canEvaluate(s) {
		return
	}
	llmobs.SubmitEvaluationFromSpan(label, value, s.obs, t.evalOpts(tags)...)
}

// EvalCategorical submits a string-valued evaluation against a span.
func (t *Tracer) EvalCategorical(s *Span, label, value string, tags map[string]string) {
	if !t.canEvaluate(s) || value == "" {
		return
	}
	llmobs.SubmitEvaluationFromSpan(label, value, s.obs, t.evalOpts(tags)...)
}

// EvalBool submits a boolean evaluation against a span.
func (t *Tracer) EvalBool(s *Span, label string, value bool, tags map[string]string) {
	if !t.canEvaluate(s) {
		return
	}
	llmobs.SubmitEvaluationFromSpan(label, value, s.obs, t.evalOpts(tags)...)
}
