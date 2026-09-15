package weaviate

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/weaviate/weaviate-go-client/v5/weaviate"
	"github.com/weaviate/weaviate-go-client/v5/weaviate/graphql"
	"go.opentelemetry.io/otel/codes"

	"github.com/braintrustdata/braintrust-sdk-go/internal/oteltest"
	"github.com/braintrustdata/braintrust-sdk-go/internal/vcr"
)

const testClassName = "Article"

var testVector = []float32{0.1, 0.2, 0.3, 0.4, 0.5}

// setUpTest is a helper function that sets up a new tracer provider and VCR
// for each test. It returns a Weaviate client configured with tracing and
// VCR, and the span exporter.
func setUpTest(t *testing.T) (*weaviate.Client, *oteltest.Exporter) {
	t.Helper()

	tp, exporter := oteltest.Setup(t)

	mode := vcr.GetVCRMode()

	// The local Weaviate instance proxies generation calls to OpenAI, so the
	// generative-openai module needs a real OpenAI key to record against.
	openAIKey := os.Getenv("OPENAI_API_KEY")
	if mode != vcr.ModeReplay && openAIKey == "" {
		t.Fatal("OPENAI_API_KEY not set (required in record/off mode)")
	}
	if openAIKey == "" {
		openAIKey = "dummy-openai-key-for-replay"
	}

	httpClient := vcr.NewHTTPClient(t)
	tracedClient := WrapClient(httpClient, WithTracerProvider(tp))

	client, err := weaviate.NewClient(weaviate.Config{
		Host:             "localhost:8090",
		Scheme:           "http",
		ConnectionClient: tracedClient,
		Headers:          map[string]string{"X-OpenAI-Api-Key": openAIKey},
	})
	require.NoError(t, err)

	return client, exporter
}

func articleFields() []graphql.Field {
	return []graphql.Field{{Name: "title"}, {Name: "content"}}
}

func TestGenerativeSearchSingleResult(t *testing.T) {
	client, exporter := setUpTest(t)
	require := require.New(t)
	assert := assert.New(t)

	nearVector := client.GraphQL().NearVectorArgBuilder().WithVector(testVector)
	gs := graphql.NewGenerativeSearch().SingleResult("Summarize this in one sentence: {content}")

	resp, err := client.GraphQL().Get().
		WithClassName(testClassName).
		WithFields(articleFields()...).
		WithNearVector(nearVector).
		WithGenerativeSearch(gs).
		Do(context.Background())
	require.NoError(err)
	require.NotNil(resp)
	require.Empty(resp.Errors)

	ts := exporter.FlushOne()
	ts.AssertNameIs("weaviate.graphql.generate")
	assert.Equal(codes.Unset, ts.Stub.Status.Code)

	ts.AssertJSONAttrEquals("braintrust.span_attributes", map[string]any{"type": "llm"})

	metadata := ts.Metadata()
	assert.Equal("weaviate", metadata["provider"])
	assert.Equal(testClassName, metadata["class_name"])

	inputRaw, ok := ts.Input().(map[string]any)
	require.True(ok)
	query, ok := inputRaw["query"].(string)
	require.True(ok)
	assert.Contains(query, "generate(")

	output, ok := ts.Output().([]any)
	require.True(ok, "expected an array of retrieved objects")
	require.NotEmpty(output)
	obj, ok := output[0].(map[string]any)
	require.True(ok)
	additional, ok := obj["_additional"].(map[string]any)
	require.True(ok)
	generate, ok := additional["generate"].(map[string]any)
	require.True(ok)
	assert.NotEmpty(generate["singleResult"])
	assert.Nil(generate["error"])
}

func TestGenerativeSearchGroupedResult(t *testing.T) {
	client, exporter := setUpTest(t)
	require := require.New(t)
	assert := assert.New(t)

	nearVector := client.GraphQL().NearVectorArgBuilder().WithVector(testVector)
	gs := graphql.NewGenerativeSearch().GroupedResult("Write a one-sentence tweet about this.")

	resp, err := client.GraphQL().Get().
		WithClassName(testClassName).
		WithFields(articleFields()...).
		WithNearVector(nearVector).
		WithGenerativeSearch(gs).
		Do(context.Background())
	require.NoError(err)
	require.NotNil(resp)

	ts := exporter.FlushOne()
	ts.AssertNameIs("weaviate.graphql.generate")

	output, ok := ts.Output().([]any)
	require.True(ok)
	require.NotEmpty(output)
	obj, ok := output[0].(map[string]any)
	require.True(ok)
	additional, ok := obj["_additional"].(map[string]any)
	require.True(ok)
	generate, ok := additional["generate"].(map[string]any)
	require.True(ok)
	assert.NotEmpty(generate["groupedResult"])
}

// TestPlainVectorSearchNotTraced exercises a GraphQL Get query with no
// WithGenerativeSearch - plain vector search is out of scope (it's not a
// generative-AI execution surface), so it must produce no span at all.
func TestPlainVectorSearchNotTraced(t *testing.T) {
	client, exporter := setUpTest(t)
	require := require.New(t)

	nearVector := client.GraphQL().NearVectorArgBuilder().WithVector(testVector)

	resp, err := client.GraphQL().Get().
		WithClassName(testClassName).
		WithFields(articleFields()...).
		WithNearVector(nearVector).
		Do(context.Background())
	require.NoError(err)
	require.NotNil(resp)

	require.Empty(exporter.Flush(), "plain vector search must not produce a span")
}
