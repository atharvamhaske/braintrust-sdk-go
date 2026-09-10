package tracea2a

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"

	"github.com/braintrustdata/braintrust-sdk-go/internal/oteltest"
)

// A2A tests use a real local httptest server speaking JSON-RPC rather than
// VCR: both the client and server are this SDK's own code, so there is no
// external API to record a cassette against.

type echoExecutor struct{}

func (echoExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	reply := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "hi " + textOf(reqCtx.Message)})
	return q.Write(ctx, reply)
}

func (echoExecutor) Cancel(_ context.Context, _ *a2asrv.RequestContext, _ eventqueue.Queue) error {
	return nil
}

func textOf(msg *a2a.Message) string {
	if msg == nil {
		return ""
	}
	for _, part := range msg.Parts {
		if text, ok := part.(a2a.TextPart); ok {
			return text.Text
		}
	}
	return ""
}

func setupServer(t *testing.T) *httptest.Server {
	t.Helper()
	handler := a2asrv.NewHandler(echoExecutor{}, InstrumentServer())
	mux := http.NewServeMux()
	mux.Handle("/invoke", a2asrv.NewJSONRPCHandler(handler))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func setupClient(t *testing.T, serverURL string) *a2aclient.Client {
	t.Helper()
	client, err := a2aclient.NewFromEndpoints(t.Context(), []a2a.AgentInterface{
		{Transport: a2a.TransportProtocolJSONRPC, URL: serverURL + "/invoke"},
	})
	require.NoError(t, err)
	InstrumentClient(client)
	t.Cleanup(func() { _ = client.Destroy() })
	return client
}

func setupOtel(t *testing.T) *oteltest.Exporter {
	t.Helper()
	tp, exporter := oteltest.Setup(t)
	original := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(original) })
	return exporter
}

func TestInstrumentClient_SendMessage(t *testing.T) {
	exporter := setupOtel(t)
	server := setupServer(t)
	client := setupClient(t, server.URL)

	result, err := client.SendMessage(context.Background(), &a2a.MessageSendParams{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "Braintrust"}),
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	spans := exporter.Flush()
	clientSpan := findSpanWithRole(t, spans, "a2a.SendMessage", "client")
	require.Equal(t, "a2a", clientSpan.Metadata()["provider"])
	require.Contains(t, clientSpan.Attr("braintrust.output_json").String(), "hi Braintrust")

	serverSpan := findSpanWithRole(t, spans, "a2a.OnSendMessage", "server")
	require.Equal(t, "server", serverSpan.Metadata()["role"])
}

func TestInstrumentClient_SendStreamingMessage(t *testing.T) {
	exporter := setupOtel(t)
	server := setupServer(t)
	client := setupClient(t, server.URL)

	var gotFinal bool
	for event, err := range client.SendStreamingMessage(context.Background(), &a2a.MessageSendParams{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "Braintrust"}),
	}) {
		require.NoError(t, err)
		require.NotNil(t, event)
		gotFinal = true
	}
	require.True(t, gotFinal, "should have received at least one event")

	// The span must be ended (exported) even though echoExecutor's reply is a
	// single terminal Message rather than a TaskStatusUpdateEvent - this is
	// the case the isTerminalEvent fix covers.
	spans := exporter.Flush()
	clientSpan := findSpanWithRole(t, spans, "a2a.SendStreamingMessage", "client")
	require.Contains(t, clientSpan.Attr("braintrust.output_json").String(), "hi Braintrust")
}

func TestInstrumentServer_TracesErrors(t *testing.T) {
	exporter := setupOtel(t)
	server := setupServer(t)
	client := setupClient(t, server.URL)

	_, err := client.GetTask(context.Background(), &a2a.TaskQueryParams{ID: "does-not-exist"})
	require.Error(t, err)

	spans := exporter.Flush()
	clientSpan := findSpanWithRole(t, spans, "a2a.GetTask", "client")
	require.Equal(t, "a2a", clientSpan.Metadata()["provider"])
}

func findSpanWithRole(t *testing.T, spans []oteltest.Span, name, role string) oteltest.Span {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name && span.Metadata()["role"] == role {
			return span
		}
	}
	t.Fatalf("no span named %q with role %q found among %d spans", name, role, len(spans))
	return oteltest.Span{}
}
