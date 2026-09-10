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

// greetExecutor is a minimal AgentExecutor that echoes a greeting back.
type greetExecutor struct{}

func (greetExecutor) Execute(ctx context.Context, reqCtx *a2asrv.RequestContext, q eventqueue.Queue) error {
	name := "there"
	if reqCtx.Message != nil {
		for _, part := range reqCtx.Message.Parts {
			if text, ok := part.(a2a.TextPart); ok {
				name = text.Text
			}
		}
	}
	reply := a2a.NewMessage(a2a.MessageRoleAgent, a2a.TextPart{Text: "Hi " + name})
	return q.Write(ctx, reply)
}

func (greetExecutor) Cancel(_ context.Context, _ *a2asrv.RequestContext, _ eventqueue.Queue) error {
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

	tracer := otel.Tracer("a2a-internal-example")
	ctx, span := tracer.Start(context.Background(), "examples/internal/a2a/main.go")
	defer span.End()

	// A2A has no external provider API: both the "remote" agent server and
	// the client talking to it are this example's own code, run over a real
	// local HTTP server.
	handler := a2asrv.NewHandler(greetExecutor{}, tracea2a.InstrumentServer())
	mux := http.NewServeMux()
	mux.Handle("/invoke", a2asrv.NewJSONRPCHandler(handler))
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := a2aclient.NewFromEndpoints(ctx, []a2a.AgentInterface{
		{Transport: a2a.TransportProtocolJSONRPC, URL: server.URL + "/invoke"},
	})
	if err != nil {
		log.Fatalf("create client: %v", err)
	}
	tracea2a.InstrumentClient(client)
	defer func() { _ = client.Destroy() }()

	result, err := client.SendMessage(ctx, &a2a.MessageSendParams{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "Braintrust"}),
	})
	if err != nil {
		log.Fatalf("SendMessage: %v", err)
	}
	if msg, ok := result.(*a2a.Message); ok {
		for _, part := range msg.Parts {
			if text, ok := part.(a2a.TextPart); ok {
				fmt.Printf("SendMessage -> %s\n", text.Text)
			}
		}
	}

	fmt.Printf("View trace: %s\n", bt.Permalink(span))
}
