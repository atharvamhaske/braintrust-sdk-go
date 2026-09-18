// TypeSafe AI (Jev) example - traces SystemOne calls to the unofficial Go SDK.
package main

import (
	"context"
	"fmt"
	"log"

	ts "github.com/atharvamhaske/typesafe-sdk-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/braintrustdata/braintrust-sdk-go"
	tracetypesafe "github.com/braintrustdata/braintrust-sdk-go/trace/contrib/typesafe"
)

func main() {
	ctx := context.Background()

	tp := trace.NewTracerProvider()
	defer func() {
		if err := tp.Shutdown(ctx); err != nil {
			log.Printf("failed to shut down tracer provider: %v", err)
		}
	}()
	otel.SetTracerProvider(tp)

	bt, err := braintrust.New(tp,
		braintrust.WithProject("typesafe-examples"),
		braintrust.WithBlockingLogin(true),
	)
	if err != nil {
		log.Fatal(err)
	}

	client, err := ts.NewClient(ts.WithHTTPClient(tracetypesafe.Client()))
	if err != nil {
		log.Fatal(err)
	}

	// Same scenario as the official Python SDK's TypeSafe integration test
	// (test_setup_typesafe_system_one_async in braintrust-sdk-python#777),
	// adapted to Go's typed Question fields.
	tracer := otel.Tracer("typesafe-example")
	ctx, span := tracer.Start(ctx, "Support ticket triage")
	defer span.End()

	resp, err := client.SystemOne(ctx,
		"I was charged twice. Please refund the duplicate charge today.",
		map[string]ts.Question{
			"category": ts.Choice{
				Instructions: "Which team should handle this?",
				Criteria: map[string]string{
					"billing":   "",
					"technical": "",
					"other":     "",
				},
			},
			"urgency": ts.Score{
				Instructions: "How urgent is this request?",
				Criteria:     []string{"routine", "soon", "urgent"},
			},
			"duplicate_charge": ts.Noul{
				Instructions: "Does the customer report a duplicate charge?",
				Criteria:     map[string]string{},
			},
		},
	)
	if err != nil {
		log.Fatal(err)
	}

	for name, answer := range resp.Choices() {
		fmt.Printf("%s: %s (confidence %.2f)\n", name, answer.Choice, answer.Confidence)
	}
	for name, answer := range resp.Scores() {
		fmt.Printf("%s: %.1f\n", name, answer.Score)
	}
	for name, answer := range resp.Nouls() {
		fmt.Printf("%s: %.2f\n", name, answer.Noul)
	}

	fmt.Printf("View trace: %s\n", bt.Permalink(span))
}
