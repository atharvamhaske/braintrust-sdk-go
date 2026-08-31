package evalrunner

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bt deserializes our `summary` frame into a strict camelCase struct with no
// snake_case aliases, and DROPS THE EVENT SILENTLY if it does not fit --
// there is no error logged anywhere in bt, and the run just reports
// "Eval runner did not return a summary".
//
// This test is the guard against that: it pins the exact wire shape against
// bt/src/eval.rs:2763 (ExperimentSummary) and :2790 (ScoreSummary).
func TestSummaryEvent_MatchesBtWireFormat(t *testing.T) {
	encoded, err := json.Marshal(summaryEvent{
		ProjectName:    "go-sdk-examples",
		ExperimentName: "food-classifier",
		ProjectID:      "proj-1",
		ExperimentID:   "exp-1",
		ExperimentURL:  "https://www.braintrust.dev/app/acme/object?object_type=experiment&object_id=exp-1",
		Scores: map[string]scoreSummary{
			"exact_match": {Name: "exact_match", Score: 0.8},
		},
	})
	require.NoError(t, err)

	assert.JSONEq(t, `{
		"projectName": "go-sdk-examples",
		"experimentName": "food-classifier",
		"projectId": "proj-1",
		"experimentId": "exp-1",
		"experimentUrl": "https://www.braintrust.dev/app/acme/object?object_type=experiment&object_id=exp-1",
		"scores": {
			"exact_match": {"name": "exact_match", "score": 0.8, "improvements": 0, "regressions": 0}
		}
	}`, string(encoded))
}

// bt's `scores` field is a required HashMap. A JSON null fails to deserialize,
// which takes the whole summary down with it, so an eval that produced no
// scores must still emit an empty object.
func TestSummaryEvent_ScoresNeverSerializeAsNull(t *testing.T) {
	encoded, err := json.Marshal(newSummaryEvent(nil, nil))
	require.NoError(t, err)

	assert.Contains(t, string(encoded), `"scores":{}`)
	assert.NotContains(t, string(encoded), `"scores":null`)
}

// bt requires projectName and experimentName -- they have no serde default, so
// omitting them drops the event. They must never carry omitempty.
func TestSummaryEvent_RequiredFieldsAlwaysPresent(t *testing.T) {
	encoded, err := json.Marshal(summaryEvent{})
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	assert.Contains(t, decoded, "projectName")
	assert.Contains(t, decoded, "experimentName")
	assert.Contains(t, decoded, "scores")
}

func TestNewSummaryEvent_AveragesScoresAcrossCases(t *testing.T) {
	acc := newScoreAccumulator()
	acc.add(map[string]float64{"exact_match": 1, "valid_category": 1})
	acc.add(map[string]float64{"exact_match": 0, "valid_category": 1})

	summary := newSummaryEvent(acc.averages(), nil)

	assert.Equal(t, scoreSummary{Name: "exact_match", Score: 0.5}, summary.Scores["exact_match"])
	assert.Equal(t, scoreSummary{Name: "valid_category", Score: 1}, summary.Scores["valid_category"])
}

// bt's SseProgressEventData requires every field except origin, and `data` must
// be a string -- it carries JSON-encoded JSON. See bt/src/eval.rs:2820.
func TestProgressEvent_MatchesBtWireFormat(t *testing.T) {
	encoded, err := json.Marshal(progressEvent{
		ID:         "span-1",
		ObjectType: "task",
		Name:       "food-classifier",
		Format:     "code",
		OutputType: "completion",
		Event:      "json_delta",
		Data:       `"fruit"`,
	})
	require.NoError(t, err)

	assert.JSONEq(t, `{
		"id": "span-1",
		"object_type": "task",
		"name": "food-classifier",
		"format": "code",
		"output_type": "completion",
		"event": "json_delta",
		"data": "\"fruit\""
	}`, string(encoded))
}

// The per-case completion frame carries no payload. `data` must still be
// present as an empty string: bt's struct has no default for it, so dropping
// the key discards the frame.
func TestProgressEvent_EmptyDataStillSerializes(t *testing.T) {
	encoded, err := json.Marshal(progressEvent{
		ID:         "span-1",
		ObjectType: "task",
		Name:       "food-classifier",
		Format:     "code",
		OutputType: "completion",
		Event:      "done",
	})
	require.NoError(t, err)

	assert.Contains(t, string(encoded), `"data":""`)
}

func TestStartEvent_MatchesBtWireFormat(t *testing.T) {
	encoded, err := json.Marshal(startEvent{
		ProjectName:    "go-sdk-examples",
		ExperimentName: "food-classifier",
		ProjectID:      "proj-1",
		ExperimentID:   "exp-1",
		ExperimentURL:  "https://example.test/exp",
	})
	require.NoError(t, err)

	assert.JSONEq(t, `{
		"projectName": "go-sdk-examples",
		"experimentName": "food-classifier",
		"projectId": "proj-1",
		"experimentId": "exp-1",
		"experimentUrl": "https://example.test/exp"
	}`, string(encoded))
}

// bt's EvalErrorPayload requires `message`; status drives the HTTP status the
// playground sees. See bt/src/eval.rs:2799.
func TestErrorEvent_MatchesBtWireFormat(t *testing.T) {
	encoded, err := json.Marshal(errorEvent{Message: "evaluator \"nope\" not found", Status: 404})
	require.NoError(t, err)

	assert.JSONEq(t, `{"message": "evaluator \"nope\" not found", "status": 404}`, string(encoded))
}
