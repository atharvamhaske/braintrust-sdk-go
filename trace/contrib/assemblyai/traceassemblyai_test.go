package assemblyai

import (
	"context"
	"os"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/braintrustdata/braintrust-sdk-go/internal/oteltest"
	"github.com/braintrustdata/braintrust-sdk-go/internal/vcr"
)

// testModel is pinned for recorded cassettes - the model AssemblyAI's own
// LLM Gateway quickstart docs use, confirmed live via a real call.
const testModel = "qwen3.5-4b-32k-fast"

const llmGatewayBaseURL = "https://llm-gateway.assemblyai.com/v1/"

// setUpTest builds an openai-go client pointed at AssemblyAI's LLM Gateway,
// wired to the VCR cassette for the current test. LLM Gateway expects a bare
// API key in the Authorization header (no "Bearer " prefix), which
// option.WithHeader overrides openai-go's default with.
func setUpTest(t *testing.T) (openai.Client, *oteltest.Exporter) {
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

	client := openai.NewClient(
		option.WithBaseURL(llmGatewayBaseURL),
		option.WithHeader("Authorization", apiKey),
		option.WithHTTPClient(tracedClient),
	)
	return client, exporter
}

func TestChatCompletions(t *testing.T) {
	client, exporter := setUpTest(t)

	timer := oteltest.NewTimer()
	resp, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model: testModel,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("What is the capital of France? Reply in one word."),
		},
		MaxTokens: openai.Int(50),
	})
	timeRange := timer.Tick()

	require.NoError(t, err)
	require.NotEmpty(t, resp.Choices)
	assert.Contains(t, resp.Choices[0].Message.Content, "Paris")

	span := exporter.FlushOne()
	span.AssertInTimeRange(timeRange)
	span.AssertNameIs("assemblyai.chat_completions")

	metadata := span.Metadata()
	assert.Equal(t, "assemblyai", metadata["provider"])
	assert.Equal(t, testModel, metadata["model"])
	assert.Equal(t, float64(200), metadata["llm_status_code"])
	assert.NotEmpty(t, metadata["request_id"])

	input := span.Attr("braintrust.input_json").String()
	assert.Contains(t, input, "capital of France")

	output := span.Attr("braintrust.output_json").String()
	assert.Contains(t, output, "Paris")

	metrics := span.Metrics()
	assert.Greater(t, metrics["prompt_tokens"], float64(0))
	assert.Greater(t, metrics["completion_tokens"], float64(0))
	assert.Greater(t, metrics["tokens"], float64(0))
	assert.GreaterOrEqual(t, metrics["time_to_first_token"], float64(0))
}

// TestChatCompletionsError verifies error handling with an unrecognized
// model name - confirmed live (twice) that LLM Gateway rejects this with a
// real HTTP 400 ("model not found"). The span's status is NOT expected to be
// codes.Error here: internal.Middleware()'s RoundTripper only sees a Go
// error for genuine transport failures, never for HTTP 4xx/5xx responses
// (status-code -> error conversion happens one layer up, inside openai-go
// itself). This matches the established precedent in this pattern family -
// see TestErrorHandling in trace/contrib/github.com/sashabaranov/go-openai,
// which triggers a real HTTP 401 via VCR and asserts span name/metadata but
// deliberately not span status. Only a genuine transport-level failure (as
// in trace/contrib/openai's TestError, which injects a fake Go error via a
// middleware) would exercise the codes.Error branch.
func TestChatCompletionsError(t *testing.T) {
	client, exporter := setUpTest(t)

	_, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model: "bogus-nonexistent-model",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("hi"),
		},
	})
	require.Error(t, err)

	span := exporter.FlushOne()
	span.AssertNameIs("assemblyai.chat_completions")

	metadata := span.Metadata()
	assert.Equal(t, "assemblyai", metadata["provider"])
	assert.Equal(t, "bogus-nonexistent-model", metadata["model"])
}
