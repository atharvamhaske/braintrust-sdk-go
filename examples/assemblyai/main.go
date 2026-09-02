// This example demonstrates basic AssemblyAI LeMUR tracing with Braintrust.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	assemblyai "github.com/AssemblyAI/assemblyai-go-sdk"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/braintrustdata/braintrust-sdk-go"
	traceassemblyai "github.com/braintrustdata/braintrust-sdk-go/trace/contrib/assemblyai"
)

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

	// Create an AssemblyAI client with Braintrust tracing.
	client := assemblyai.NewClientWithOptions(
		assemblyai.WithAPIKey(os.Getenv("ASSEMBLYAI_API_KEY")),
		assemblyai.WithHTTPClient(traceassemblyai.Client()), // Add tracing via custom HTTP client
	)

	tracer := otel.Tracer("assemblyai-example")
	ctx, span := tracer.Start(context.Background(), "examples/assemblyai/main.go")
	defer span.End()

	// Ask LeMUR a custom task prompt against a snippet of transcript text.
	// InputText/TranscriptIDs both accept up to 100 hours of transcript
	// content; here we pass a short snippet directly.
	resp, err := client.LeMUR.Task(ctx, assemblyai.LeMURTaskParams{
		Prompt: assemblyai.String("What is the main topic discussed? Answer in one sentence."),
		LeMURBaseParams: assemblyai.LeMURBaseParams{
			InputText: assemblyai.String("Speaker A: Today we're going to talk about the quarterly roadmap. " +
				"Speaker B: Great, let's start with the mobile app redesign."),
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Response: %s\n", assemblyai.ToString(resp.Response))
	fmt.Printf("View trace: %s\n", bt.Permalink(span))
}
