package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/braintrustdata/braintrust-sdk-go/internal/oteltest"
	"github.com/braintrustdata/braintrust-sdk-go/internal/vcr"
)

// setUpTest sets up a new tracer provider and VCR for each test. It returns a
// traced HTTP client pointed at the real TypeSafe API and an exporter. This
// package has no dependency on any TypeSafe Go client (official or
// otherwise) - it traces at the HTTP layer, so tests call the API directly.
func setUpTest(t *testing.T) (*http.Client, string, *oteltest.Exporter) {
	t.Helper()

	tp, exporter := oteltest.Setup(t)

	mode := vcr.GetVCRMode()

	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if mode != vcr.ModeReplay && apiKey == "" {
		t.Fatal("TYPESAFE_API_KEY not set (required in record/off mode)")
	}
	if apiKey == "" {
		apiKey = "dummy-typesafe-key-for-replay"
	}

	httpClient := vcr.NewHTTPClient(t)
	tracedClient := WrapClient(httpClient, WithTracerProvider(tp))

	return tracedClient, apiKey, exporter
}

// TestSystemOne mirrors the "Support ticket triage" example from the
// official Python SDK's TypeSafe integration (braintrust-sdk-python#777).
func TestSystemOne(t *testing.T) {
	client, apiKey, exporter := setUpTest(t)
	assert := assert.New(t)
	require := require.New(t)

	reqBody := map[string]any{
		"state": "Customer says they were charged twice for order #4471 and want it fixed today.",
		"model": "jev-latest",
		"questions": map[string]any{
			"category": map[string]any{
				"type":         "choice",
				"instructions": "What is this ticket about?",
				"criteria": map[string]string{
					"billing":   "Charges, refunds, or payment issues",
					"shipping":  "Delivery or tracking issues",
					"technical": "Product doesn't work as expected",
				},
			},
			"urgency": map[string]any{
				"type":         "score",
				"instructions": "How urgent is this ticket, from 1 (low) to 5 (high)?",
				"criteria":     []string{"1", "2", "3", "4", "5"},
			},
			"duplicate_charge": map[string]any{
				"type":         "noul",
				"instructions": "Is the customer reporting a duplicate charge?",
				"criteria": map[string]string{
					"true":  "Customer was billed more than once for the same order",
					"false": "No duplicate billing mentioned",
				},
			},
		},
	}
	body, err := json.Marshal(reqBody)
	require.NoError(err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://api.typesafe.ai/v1/systemone", bytes.NewReader(body))
	require.NoError(err)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode)

	var respBody map[string]any
	decodeErr := json.NewDecoder(resp.Body).Decode(&respBody)
	require.NoError(resp.Body.Close()) // triggers span.End() - see BufferedReader in trace/internal/middleware.go
	require.NoError(decodeErr)
	answers, ok := respBody["answers"].(map[string]any)
	require.True(ok)
	assert.Len(answers, 3)

	ts := exporter.FlushOne()
	assert.Equal("typesafe.systemOne", ts.Name())
	ts.AssertJSONAttrEquals("braintrust.span_attributes", map[string]any{"type": "question"})
	assert.Equal("typesafe", ts.Metadata()["provider"])
	assert.NotEmpty(ts.Metadata()["model"])

	input, ok := ts.Input().(map[string]any)
	require.True(ok)
	assert.Equal(reqBody["state"], input["state"])
	assert.Len(input["questions"], 3)

	output, ok := ts.Output().(map[string]any)
	require.True(ok)
	outputAnswers, ok := output["answers"].([]any)
	require.True(ok)
	assert.Len(outputAnswers, 3)

	metrics := ts.Metrics()
	assert.Greater(metrics["prompt_tokens"], float64(0))
	assert.Greater(metrics["completion_tokens"], float64(0))
	assert.Equal(metrics["prompt_tokens"]+metrics["completion_tokens"], metrics["tokens"])
}
