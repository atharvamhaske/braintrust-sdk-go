// This example demonstrates basic AssemblyAI LLM Gateway tracing with Braintrust.
//
// AssemblyAI doesn't publish a Go client for LLM Gateway, so this uses the
// official openai-go client pointed at AssemblyAI's base URL - LLM Gateway's
// request/response shape is OpenAI-Chat-Completions-compatible.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/braintrustdata/braintrust-sdk-go"
	traceassemblyai "github.com/braintrustdata/braintrust-sdk-go/trace/contrib/assemblyai"
)

const llmGatewayModel = "qwen3.5-4b-32k-fast"

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

	// Create an openai-go client pointed at AssemblyAI's LLM Gateway with
	// Braintrust tracing. LLM Gateway expects a bare API key in the
	// Authorization header (no "Bearer " prefix), which WithHeader overrides
	// openai-go's default with.
	client := openai.NewClient(
		option.WithBaseURL("https://llm-gateway.assemblyai.com/v1/"),
		option.WithHeader("Authorization", os.Getenv("ASSEMBLYAI_API_KEY")),
		option.WithHTTPClient(traceassemblyai.Client()), // Add tracing via custom HTTP client
	)

	tracer := otel.Tracer("assemblyai-example")
	ctx, span := tracer.Start(context.Background(), "examples/assemblyai/main.go")
	defer span.End()

	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: llmGatewayModel,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("What is the capital of France?"),
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Response: %s\n", resp.Choices[0].Message.Content)
	fmt.Printf("View trace: %s\n", bt.Permalink(span))
}
