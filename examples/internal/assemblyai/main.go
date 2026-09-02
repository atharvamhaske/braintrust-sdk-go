// AssemblyAI LeMUR kitchen sink - exercises all four LeMUR generation endpoints.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	assemblyai "github.com/AssemblyAI/assemblyai-go-sdk"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/braintrustdata/braintrust-sdk-go"
	traceassemblyai "github.com/braintrustdata/braintrust-sdk-go/trace/contrib/assemblyai"
)

var tracer = otel.Tracer("assemblyai-examples")

// sampleTranscript stands in for a real AssemblyAI transcript. LeMUR accepts
// raw input_text directly, so this example doesn't need to upload audio or
// wait for a transcription job to complete.
const sampleTranscript = "Speaker A: Welcome to the quarterly planning meeting. " +
	"Let's start with the mobile app redesign. " +
	"Speaker B: The redesign is on track for a March launch. " +
	"We still need to finalize the onboarding flow and fix the checkout bug. " +
	"Speaker A: Great, let's make the checkout bug our top action item this sprint."

// LeMURBot demonstrates AssemblyAI's LeMUR API with tracing.
type LeMURBot struct {
	client *assemblyai.Client
}

func newLeMURBot(client *assemblyai.Client) *LeMURBot {
	return &LeMURBot{client: client}
}

// task demonstrates LeMUR's custom-prompt endpoint.
func (b *LeMURBot) task(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "task")
	defer span.End()

	fmt.Println("\n=== Example 1: Task ===")

	resp, err := b.client.LeMUR.Task(ctx, assemblyai.LeMURTaskParams{
		Prompt: assemblyai.String("What is the main topic of this meeting? Answer in one sentence."),
		LeMURBaseParams: assemblyai.LeMURBaseParams{
			InputText:   assemblyai.String(sampleTranscript),
			Temperature: assemblyai.Float64(0.2),
		},
	})
	if err != nil {
		return fmt.Errorf("task: %w", err)
	}
	fmt.Printf("  %s\n", assemblyai.ToString(resp.Response))
	return nil
}

// summarize demonstrates LeMUR's summary endpoint with a custom answer format.
func (b *LeMURBot) summarize(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "summarize")
	defer span.End()

	fmt.Println("\n=== Example 2: Summarize ===")

	resp, err := b.client.LeMUR.Summarize(ctx, assemblyai.LeMURSummaryParams{
		AnswerFormat: assemblyai.String("TLDR"),
		LeMURBaseParams: assemblyai.LeMURBaseParams{
			InputText: assemblyai.String(sampleTranscript),
		},
	})
	if err != nil {
		return fmt.Errorf("summarize: %w", err)
	}
	fmt.Printf("  %s\n", assemblyai.ToString(resp.Response))
	return nil
}

// actionItems demonstrates LeMUR's action-items endpoint.
func (b *LeMURBot) actionItems(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "action-items")
	defer span.End()

	fmt.Println("\n=== Example 3: Action Items ===")

	resp, err := b.client.LeMUR.ActionItems(ctx, assemblyai.LeMURActionItemsParams{
		LeMURBaseParams: assemblyai.LeMURBaseParams{
			InputText: assemblyai.String(sampleTranscript),
		},
	})
	if err != nil {
		return fmt.Errorf("action-items: %w", err)
	}
	fmt.Printf("  %s\n", assemblyai.ToString(resp.Response))
	return nil
}

// question demonstrates LeMUR's question-answer endpoint with multiple questions.
func (b *LeMURBot) question(ctx context.Context) error {
	ctx, span := tracer.Start(ctx, "question")
	defer span.End()

	fmt.Println("\n=== Example 4: Question & Answer ===")

	resp, err := b.client.LeMUR.Question(ctx, assemblyai.LeMURQuestionAnswerParams{
		Questions: []assemblyai.LeMURQuestion{
			{Question: assemblyai.String("When is the mobile app redesign launching?")},
			{Question: assemblyai.String("What is this sprint's top action item?")},
		},
		LeMURBaseParams: assemblyai.LeMURBaseParams{
			InputText: assemblyai.String(sampleTranscript),
		},
	})
	if err != nil {
		return fmt.Errorf("question: %w", err)
	}
	for _, qa := range resp.Response {
		fmt.Printf("  Q: %s\n  A: %s\n", assemblyai.ToString(qa.Question), assemblyai.ToString(qa.Answer))
	}
	return nil
}

func main() {
	fmt.Println("Braintrust AssemblyAI LeMUR Tracing Examples")
	fmt.Println("=============================================")

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

	client := assemblyai.NewClientWithOptions(
		assemblyai.WithAPIKey(apiKey),
		assemblyai.WithHTTPClient(traceassemblyai.Client(traceassemblyai.WithTracerProvider(tp))),
	)
	bot := newLeMURBot(client)

	ctx := context.Background()
	ctx, rootSpan := tracer.Start(ctx, "examples/internal/assemblyai/main.go")
	defer rootSpan.End()

	fmt.Println("\nAssemblyAI LeMUR Examples")
	fmt.Println(strings.Repeat("=", 25))
	fmt.Println("Demonstrating: Task, Summarize, ActionItems, and Question & Answer")

	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"task", bot.task},
		{"summarize", bot.summarize},
		{"action-items", bot.actionItems},
		{"question", bot.question},
	}
	for _, s := range steps {
		if err := s.fn(ctx); err != nil {
			log.Printf("  [%s] %v", s.name, err)
		}
	}

	fmt.Println("\n=== Tracing Complete ===")
	fmt.Printf("View trace: %s\n", bt.Permalink(rootSpan))
}
