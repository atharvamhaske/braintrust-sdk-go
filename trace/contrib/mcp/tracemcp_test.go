package tracemcp

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/internal/oteltest"
)

type greetArgs struct {
	Name string `json:"name"`
}

func setupInMemorySession(t *testing.T, tp oteltrace.TracerProvider, instrumentClient, instrumentServer bool) (*mcp.ClientSession, func()) {
	t.Helper()

	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "v1.0.0"}, nil)
	if instrumentServer {
		InstrumentServer(server, WithTracerProvider(tp))
	}
	mcp.AddTool(server, &mcp.Tool{Name: "greet", Description: "say hi"},
		func(_ context.Context, _ *mcp.CallToolRequest, args greetArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "hi " + args.Name}},
			}, nil, nil
		})

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1.0.0"}, nil)
	if instrumentClient {
		InstrumentClient(client, WithTracerProvider(tp))
	}

	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)

	cleanup := func() {
		require.NoError(t, clientSession.Close())
		require.NoError(t, serverSession.Wait())
	}
	return clientSession, cleanup
}

func TestInstrumentClient_CallTool(t *testing.T) {
	tp, exporter := oteltest.Setup(t)

	session, cleanup := setupInMemorySession(t, tp, true, false)
	defer cleanup()

	ctx := context.Background()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "greet",
		Arguments: greetArgs{Name: "world"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	spans := exporter.Flush()
	require.Len(t, spans, 1)

	span := spans[0]
	span.AssertNameIs("mcp.tools.call [greet]")
	assert.Equal(t, codes.Unset, span.Status().Code)
	span.AssertJSONAttrEquals("braintrust.span_attributes", map[string]any{"type": "tool"})
	assert.Equal(t, map[string]any{
		"name":      "greet",
		"arguments": map[string]any{"name": "world"},
	}, span.Input())
	assert.Equal(t, map[string]any{"content": "hi world"}, span.Output())
	assert.Equal(t, "mcp", span.Metadata()["provider"])
	assert.Equal(t, "client", span.Metadata()["role"])
	assert.Equal(t, "greet", span.Metadata()["name"])
}

func TestInstrumentClient_ListTools(t *testing.T) {
	tp, exporter := oteltest.Setup(t)

	session, cleanup := setupInMemorySession(t, tp, true, false)
	defer cleanup()

	ctx := context.Background()
	result, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	require.NotNil(t, result)

	spans := exporter.Flush()
	require.Len(t, spans, 1)

	span := spans[0]
	span.AssertNameIs("mcp.tools.list")
	span.AssertJSONAttrEquals("braintrust.span_attributes", map[string]any{"type": "task"})
	output, ok := span.Output().(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(1), output["count"])
}

func TestInstrumentServer_CallTool(t *testing.T) {
	tp, exporter := oteltest.Setup(t)

	session, cleanup := setupInMemorySession(t, tp, false, true)
	defer cleanup()

	ctx := context.Background()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "greet",
		Arguments: greetArgs{Name: "server"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	spans := exporter.Flush()
	require.Len(t, spans, 1)

	span := spans[0]
	span.AssertNameIs("mcp.tools.call [greet]")
	assert.Equal(t, "server", span.Metadata()["role"])
	assert.Equal(t, map[string]any{"content": "hi server"}, span.Output())
}

func TestInstrumentClient_CallToolError(t *testing.T) {
	tp, exporter := oteltest.Setup(t)

	session, cleanup := setupInMemorySession(t, tp, true, false)
	defer cleanup()

	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "missing"})
	require.Error(t, err)

	spans := exporter.Flush()
	require.Len(t, spans, 1)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
}

func TestCallToolOutput(t *testing.T) {
	t.Parallel()

	assert.Nil(t, callToolOutput(&mcp.CallToolResult{}))
	assert.Equal(t, map[string]any{
		"content": "hello",
	}, callToolOutput(&mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "hello"}},
	}))
	assert.Equal(t, map[string]any{
		"is_error": true,
		"content":  "failed",
	}, callToolOutput(&mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "failed"}},
	}))
}

func TestListToolsOutput(t *testing.T) {
	t.Parallel()

	output := listToolsOutput(&mcp.ListToolsResult{
		Tools: []*mcp.Tool{
			{Name: "alpha", Description: "first"},
			{Name: "beta"},
		},
		NextCursor: "next",
	})
	assert.Equal(t, 2, output["count"])
	assert.Equal(t, "next", output["next_cursor"])
}
