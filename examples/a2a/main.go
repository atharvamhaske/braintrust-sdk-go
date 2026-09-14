// This example demonstrates manual A2A client and server tracing with Braintrust.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2aclient"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/braintrustdata/braintrust-sdk-go"
	tracea2a "github.com/braintrustdata/braintrust-sdk-go/trace/contrib/a2a"
)

type greetingAgent struct{}

func (greetingAgent) Execute(ctx context.Context, _ *a2asrv.RequestContext, q eventqueue.Queue) error {
	return q.Write(ctx, a2a.NewMessage(
		a2a.MessageRoleAgent,
		a2a.TextPart{Text: "Hello from the A2A agent!"},
	))
}

func (greetingAgent) Cancel(context.Context, *a2asrv.RequestContext, eventqueue.Queue) error {
	return nil
}

func main() {
	tp := trace.NewTracerProvider()
	defer tp.Shutdown(context.Background()) //nolint:errcheck
	otel.SetTracerProvider(tp)

	bt, err := braintrust.New(tp,
		braintrust.WithProject("go-sdk-examples"),
		braintrust.WithBlockingLogin(true),
	)
	if err != nil {
		log.Fatal(err)
	}

	ctx, span := otel.Tracer("a2a-example").Start(context.Background(), "examples/a2a/main.go")
	defer span.End()

	// Instrument the A2A server handler.
	handler := a2asrv.NewHandler(greetingAgent{}, tracea2a.InstrumentServer())
	mux := http.NewServeMux()
	mux.Handle("/invoke", a2asrv.NewJSONRPCHandler(handler))
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := a2aclient.NewFromEndpoints(ctx, []a2a.AgentInterface{{
		Transport: a2a.TransportProtocolJSONRPC,
		URL:       server.URL + "/invoke",
	}})
	if err != nil {
		log.Fatal(err)
	}
	defer client.Destroy() //nolint:errcheck

	// Instrument all eligible direct calls made by this client.
	tracea2a.InstrumentClient(client)

	result, err := client.SendMessage(ctx, &a2a.MessageSendParams{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "Say hello"}),
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Response: %#v\n", result)
	fmt.Printf("View trace: %s\n", bt.Permalink(span))
}
