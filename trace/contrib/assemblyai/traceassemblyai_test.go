package assemblyai

import (
	"context"
	"os"
	"testing"

	assemblyai "github.com/AssemblyAI/assemblyai-go-sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"

	"github.com/braintrustdata/braintrust-sdk-go/internal/oteltest"
	"github.com/braintrustdata/braintrust-sdk-go/internal/vcr"
)

// setUpTest builds an AssemblyAI client wired to the VCR cassette for the
// current test. In replay mode (the default), a dummy API key is used since
// the cassette supplies the canned response regardless of what's sent.
func setUpTest(t *testing.T) (*assemblyai.Client, *oteltest.Exporter) {
	t.Helper()

	tp, exporter := oteltest.Setup(t)

	mode := vcr.GetVCRMode()
	apiKey := os.Getenv("ASSEMBLYAI_API_KEY")
	if mode != vcr.ModeReplay && apiKey == "" {
		t.Fatal("ASSEMBLYAI_API_KEY not set (required in record/off mode)")
	}
	if apiKey == "" {
		apiKey = "dummy-assemblyai-key-for-replay"
	}

	httpClient := vcr.NewHTTPClient(t)
	tracedClient := WrapClient(httpClient, WithTracerProvider(tp))

	client := assemblyai.NewClientWithOptions(
		assemblyai.WithAPIKey(apiKey),
		assemblyai.WithHTTPClient(tracedClient),
	)
	return client, exporter
}

func TestLeMURTask(t *testing.T) {
	client, exporter := setUpTest(t)

	timer := oteltest.NewTimer()
	out, err := client.LeMUR.Task(context.Background(), assemblyai.LeMURTaskParams{
		Prompt: assemblyai.String("What is the capital of France? Reply in one word."),
		LeMURBaseParams: assemblyai.LeMURBaseParams{
			InputText: assemblyai.String("This is a transcript about European geography."),
		},
	})
	timeRange := timer.Tick()

	require.NoError(t, err)
	require.NotNil(t, out.Response)
	assert.Contains(t, *out.Response, "Paris")

	span := exporter.FlushOne()
	span.AssertInTimeRange(timeRange)
	span.AssertNameIs("assemblyai.lemur_task")

	metadata := span.Metadata()
	assert.Equal(t, "assemblyai", metadata["provider"])
	assert.Equal(t, "task", metadata["endpoint"])
	assert.Equal(t, float64(len("This is a transcript about European geography.")), metadata["input_text_length"])

	input := span.Attr("braintrust.input_json").String()
	assert.Contains(t, input, "capital of France")
	assert.NotContains(t, input, "European geography")

	output := span.Attr("braintrust.output_json").String()
	assert.Contains(t, output, "Paris")

	metrics := span.Metrics()
	assert.Greater(t, metrics["prompt_tokens"], float64(0))
	assert.Greater(t, metrics["completion_tokens"], float64(0))
	assert.Greater(t, metrics["tokens"], float64(0))
}

func TestLeMURSummarize(t *testing.T) {
	client, exporter := setUpTest(t)

	timer := oteltest.NewTimer()
	out, err := client.LeMUR.Summarize(context.Background(), assemblyai.LeMURSummaryParams{
		AnswerFormat: assemblyai.String("TLDR"),
		LeMURBaseParams: assemblyai.LeMURBaseParams{
			InputText: assemblyai.String("A long transcript about quarterly earnings."),
		},
	})
	timeRange := timer.Tick()

	require.NoError(t, err)
	require.NotNil(t, out.Response)

	span := exporter.FlushOne()
	span.AssertInTimeRange(timeRange)
	span.AssertNameIs("assemblyai.lemur_summarize")

	metadata := span.Metadata()
	assert.Equal(t, "summary", metadata["endpoint"])

	input := span.Attr("braintrust.input_json").String()
	assert.Contains(t, input, "TLDR")
}

func TestLeMURActionItems(t *testing.T) {
	client, exporter := setUpTest(t)

	timer := oteltest.NewTimer()
	out, err := client.LeMUR.ActionItems(context.Background(), assemblyai.LeMURActionItemsParams{
		LeMURBaseParams: assemblyai.LeMURBaseParams{
			InputText: assemblyai.String("Discussed launching the new feature next sprint."),
		},
	})
	timeRange := timer.Tick()

	require.NoError(t, err)
	require.NotNil(t, out.Response)

	span := exporter.FlushOne()
	span.AssertInTimeRange(timeRange)
	span.AssertNameIs("assemblyai.lemur_action_items")

	metadata := span.Metadata()
	assert.Equal(t, "action_items", metadata["endpoint"])
}

func TestLeMURQuestion(t *testing.T) {
	client, exporter := setUpTest(t)

	timer := oteltest.NewTimer()
	out, err := client.LeMUR.Question(context.Background(), assemblyai.LeMURQuestionAnswerParams{
		Questions: []assemblyai.LeMURQuestion{
			{Question: assemblyai.String("What is the capital of France?")},
		},
		LeMURBaseParams: assemblyai.LeMURBaseParams{
			InputText: assemblyai.String("This is a transcript about European geography."),
		},
	})
	timeRange := timer.Tick()

	require.NoError(t, err)
	require.Len(t, out.Response, 1)
	assert.Contains(t, *out.Response[0].Answer, "Paris")

	span := exporter.FlushOne()
	span.AssertInTimeRange(timeRange)
	span.AssertNameIs("assemblyai.lemur_question_answer")

	metadata := span.Metadata()
	assert.Equal(t, "question_answer", metadata["endpoint"])

	input := span.Attr("braintrust.input_json").String()
	assert.Contains(t, input, "capital of France")

	output := span.Attr("braintrust.output_json").String()
	assert.Contains(t, output, "Paris")

	metrics := span.Metrics()
	assert.Greater(t, metrics["prompt_tokens"], float64(0))
}

// TestLeMURTaskError verifies error handling on a representative LeMUR
// endpoint. The error path is shared middleware code (internal.Middleware()),
// identical regardless of which LeMUR endpoint triggered it, so this single
// test covers all four.
func TestLeMURTaskError(t *testing.T) {
	client, exporter := setUpTest(t)

	_, err := client.LeMUR.Task(context.Background(), assemblyai.LeMURTaskParams{
		Prompt: assemblyai.String("hi"),
		LeMURBaseParams: assemblyai.LeMURBaseParams{
			FinalModel: assemblyai.LeMURModel("bogus/nonexistent-model"),
			InputText:  assemblyai.String("This is a transcript."),
		},
	})
	require.Error(t, err)

	span := exporter.FlushOne()
	span.AssertNameIs("assemblyai.lemur_task")
	assert.Equal(t, codes.Error, span.Stub.Status.Code)
}
