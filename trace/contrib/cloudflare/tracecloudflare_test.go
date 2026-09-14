package cloudflare

import (
	"context"
	"os"
	"testing"

	cf "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/ai"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/shared"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"

	"github.com/braintrustdata/braintrust-sdk-go/internal/oteltest"
	"github.com/braintrustdata/braintrust-sdk-go/internal/vcr"
)

const testChatModel = "@cf/meta/llama-3.1-8b-instruct-fast"
const testEmbeddingModel = "@cf/baai/bge-base-en-v1.5"

// setUpTest sets up a new tracer provider and VCR for each test. It returns
// a Cloudflare client configured with tracing and VCR, the account ID to
// use, and the span exporter.
func setUpTest(t *testing.T) (*cf.Client, string, *oteltest.Exporter) {
	t.Helper()

	tp, exporter := oteltest.Setup(t)

	mode := vcr.GetVCRMode()
	apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	if mode != vcr.ModeReplay && (apiToken == "" || accountID == "") {
		t.Fatal("CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID not set (required in record/off mode)")
	}
	if apiToken == "" {
		apiToken = "dummy-cloudflare-token-for-replay"
	}
	if accountID == "" {
		// Unlike other providers, Workers AI puts the account ID in the URL
		// path itself, so VCR's URL-based cassette matching needs the exact
		// same value at record and replay time. After recording with a real
		// CLOUDFLARE_ACCOUNT_ID, replace it in the cassette file with this
		// placeholder before committing.
		accountID = "dummy-account-id-for-replay"
	}

	httpClient := vcr.NewHTTPClient(t)

	client := cf.NewClient(
		option.WithAPIToken(apiToken),
		option.WithHTTPClient(httpClient),
		option.WithMiddleware(NewMiddleware(WithTracerProvider(tp))), //nolint:bodyclose // false positive - NewMiddleware returns middleware func
	)

	return client, accountID, exporter
}

func TestTextGeneration(t *testing.T) {
	client, accountID, exporter := setUpTest(t)

	resp, err := client.AI.Run(context.Background(), testChatModel, ai.AIRunParams{
		AccountID: cf.F(accountID),
		Body: ai.AIRunParamsBodyTextGeneration{
			Messages: cf.F([]ai.AIRunParamsBodyTextGenerationMessage{
				{
					Role:    cf.F("user"),
					Content: cf.F[ai.AIRunParamsBodyTextGenerationMessagesContentUnion](shared.UnionString("Say hello")),
				},
			}),
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	ts := exporter.FlushOne()
	ts.AssertNameIs("cloudflare.ai.text_generation")
	assert.Equal(t, codes.Unset, ts.Stub.Status.Code)

	ts.AssertJSONAttrEquals("braintrust.span_attributes", map[string]any{"type": "llm"})

	metadata := ts.Metadata()
	assert.Equal(t, "cloudflare", metadata["provider"])
	assert.Equal(t, testChatModel, metadata["model"])
	assert.Equal(t, "text_generation", metadata["task"])

	output, ok := ts.Output().(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, output["response"])

	metrics := ts.Metrics()
	assert.Greater(t, metrics["tokens"], float64(0))
}

func TestTextEmbeddings(t *testing.T) {
	client, accountID, exporter := setUpTest(t)

	resp, err := client.AI.Run(context.Background(), testEmbeddingModel, ai.AIRunParams{
		AccountID: cf.F(accountID),
		Body: ai.AIRunParamsBodyTextEmbeddings{
			Text: cf.F[ai.AIRunParamsBodyTextEmbeddingsTextUnion](shared.UnionString("hello world")),
		},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	ts := exporter.FlushOne()
	ts.AssertNameIs("cloudflare.ai.text_embeddings")

	output, ok := ts.Output().(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(1), output["count"])

	metadata := ts.Metadata()
	assert.Equal(t, "cloudflare", metadata["provider"])
	assert.Equal(t, testEmbeddingModel, metadata["model"])
	assert.Equal(t, "text_embeddings", metadata["task"])
}

func TestClassifyAIRunTask(t *testing.T) {
	assert.Equal(t, taskTextGeneration, classifyAIRunTask(map[string]any{"messages": []any{}}))
	assert.Equal(t, taskMultimodalEmbedding, classifyAIRunTask(map[string]any{"image": "abc", "text": []any{"a"}}))
	assert.Equal(t, taskTextEmbeddings, classifyAIRunTask(map[string]any{"text": "hello"}))
}
