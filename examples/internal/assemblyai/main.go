// AssemblyAI LLM Gateway kitchen sink - basic chat completion and tool calling.
//
// AssemblyAI doesn't publish a Go client for LLM Gateway, so this uses the
// official openai-go client pointed at AssemblyAI's base URL. Streaming is
// out of scope for this integration (see package doc in
// trace/contrib/assemblyai) so it's not demonstrated here.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/shared"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/braintrustdata/braintrust-sdk-go"
	traceassemblyai "github.com/braintrustdata/braintrust-sdk-go/trace/contrib/assemblyai"
)

var tracer = otel.Tracer("assemblyai-examples")

const llmGatewayModel = "qwen3.5-4b-32k-fast"

// GatewayBot demonstrates AssemblyAI's LLM Gateway with tracing.
type GatewayBot struct {
	client openai.Client
}

func newGatewayBot(client openai.Client) *GatewayBot {
	return &GatewayBot{client: client}
}

// basicChat demonstrates a plain non-streaming chat completion.
func (b *GatewayBot) basicChat(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "basic-chat")
	defer span.End()

	fmt.Println("\n=== Example 1: Basic Chat ===")

	resp, err := b.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: llmGatewayModel,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("What is the capital of France? Reply in one word."),
		},
	})
	if err != nil {
		return fmt.Errorf("basic-chat: %w", err)
	}
	fmt.Printf("  %s\n", resp.Choices[0].Message.Content)
	return nil
}

// toolCalling demonstrates a full agentic turn: model requests a tool call,
// we execute it locally, then send the result back for a final answer.
func (b *GatewayBot) toolCalling(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "tool-calling")
	defer span.End()

	fmt.Println("\n=== Example 2: Tool Calling ===")

	weatherTool := openai.ChatCompletionToolParam{
		Function: shared.FunctionDefinitionParam{
			Name:        "get_weather",
			Description: openai.String("Get the current weather for a city"),
			Parameters: openai.FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"location": map[string]any{
						"type":        "string",
						"description": "The city name",
					},
				},
				"required": []string{"location"},
			},
		},
	}

	messages := []openai.ChatCompletionMessageParamUnion{
		openai.UserMessage("What's the weather in San Francisco?"),
	}

	first, err := b.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    llmGatewayModel,
		Messages: messages,
		Tools:    []openai.ChatCompletionToolParam{weatherTool},
	})
	if err != nil {
		return fmt.Errorf("tool-calling (first call): %w", err)
	}

	toolCalls := first.Choices[0].Message.ToolCalls
	if len(toolCalls) == 0 {
		fmt.Printf("  (model answered directly, no tool call): %s\n", first.Choices[0].Message.Content)
		return nil
	}

	messages = append(messages, first.Choices[0].Message.ToParam())
	for _, tc := range toolCalls {
		fmt.Printf("  Tool: %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
		// A real implementation would parse tc.Function.Arguments and call
		// the actual weather API; this example returns a canned result.
		result, _ := json.Marshal(map[string]any{"location": "San Francisco", "temperature_f": 62, "condition": "foggy"})
		messages = append(messages, openai.ToolMessage(string(result), tc.ID))
	}

	final, err := b.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    llmGatewayModel,
		Messages: messages,
		Tools:    []openai.ChatCompletionToolParam{weatherTool},
	})
	if err != nil {
		return fmt.Errorf("tool-calling (final call): %w", err)
	}
	fmt.Printf("  Final: %s\n", final.Choices[0].Message.Content)
	return nil
}

func main() {
	fmt.Println("Braintrust AssemblyAI LLM Gateway Tracing Examples")
	fmt.Println("===================================================")

	apiKey := os.Getenv("ASSEMBLYAI_API_KEY")
	if apiKey == "" {
		log.Println("skipping assemblyai internal example: ASSEMBLYAI_API_KEY not set")
		return
	}

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

	client := openai.NewClient(
		option.WithBaseURL("https://llm-gateway.assemblyai.com/v1/"),
		option.WithHeader("Authorization", apiKey),
		option.WithHTTPClient(traceassemblyai.Client(traceassemblyai.WithTracerProvider(tp))),
	)
	bot := newGatewayBot(client)

	ctx := context.Background()
	ctx, rootSpan := tracer.Start(ctx, "examples/internal/assemblyai/main.go")
	defer rootSpan.End()

	fmt.Println("\nAssemblyAI LLM Gateway Examples")
	fmt.Println("================================")
	fmt.Println("Demonstrating: basic chat completion and tool calling")

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"basic-chat", bot.basicChat},
		{"tool-calling", bot.toolCalling},
	}
	for _, s := range steps {
		if err := s.fn(ctx); err != nil {
			log.Printf("  [%s] %v", s.name, err)
		}
	}

	fmt.Println("\n=== Tracing Complete ===")
	fmt.Printf("View trace: %s\n", bt.Permalink(rootSpan))
}
