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

	// "Support ticket triage" example - mirrors the one used by the official
	// Python and JS SDKs' TypeSafe integrations.
	tracer := otel.Tracer("typesafe-example")
	ctx, span := tracer.Start(ctx, "Support ticket triage")
	defer span.End()

	resp, err := client.SystemOne(ctx,
		"Customer says they were charged twice for order #4471 and want it fixed today.",
		map[string]ts.Question{
			"category": ts.Choice{
				Instructions: "What is this ticket about?",
				Criteria: map[string]string{
					"billing":   "Charges, refunds, or payment issues",
					"shipping":  "Delivery or tracking issues",
					"technical": "Product doesn't work as expected",
				},
			},
			"urgency": ts.Score{
				Instructions: "How urgent is this ticket, from 1 (low) to 5 (high)?",
				Criteria:     []string{"1", "2", "3", "4", "5"},
			},
			"duplicate_charge": ts.Noul{
				Instructions: "Is the customer reporting a duplicate charge?",
				Criteria: map[string]string{
					"true":  "Customer was billed more than once for the same order",
					"false": "No duplicate billing mentioned",
				},
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
