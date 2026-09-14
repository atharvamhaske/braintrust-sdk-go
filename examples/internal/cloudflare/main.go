// Cloudflare Workers AI - text generation and text embeddings with tracing
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	cf "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/ai"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/shared"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/braintrustdata/braintrust-sdk-go"
	tracecloudflare "github.com/braintrustdata/braintrust-sdk-go/trace/contrib/cloudflare"
)

var tracer = otel.Tracer("cloudflare-examples")

const chatModel = "@cf/meta/llama-3.1-8b-instruct-fast"
const embeddingModel = "@cf/baai/bge-base-en-v1.5"

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

	client := cf.NewClient(
		option.WithAPIToken(os.Getenv("CLOUDFLARE_API_TOKEN")),
		option.WithMiddleware(tracecloudflare.NewMiddleware()),
	)
	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")

	ctx, rootSpan := tracer.Start(context.Background(), "examples/internal/cloudflare/main.go")
	defer rootSpan.End()

	chatResp, err := client.AI.Run(ctx, chatModel, ai.AIRunParams{
		AccountID: cf.F(accountID),
		Body: ai.AIRunParamsBodyTextGeneration{
			Messages: cf.F([]ai.AIRunParamsBodyTextGenerationMessage{
				{
					Role:    cf.F("user"),
					Content: cf.F[ai.AIRunParamsBodyTextGenerationMessagesContentUnion](shared.UnionString("Say hello")),
				},
			}),
		},
	})
	if err != nil {
		log.Fatalf("Text generation: %v", err)
	}
	fmt.Printf("Text generation -> %+v\n", *chatResp)

	embedResp, err := client.AI.Run(ctx, embeddingModel, ai.AIRunParams{
		AccountID: cf.F(accountID),
		Body: ai.AIRunParamsBodyTextEmbeddings{
			Text: cf.F[ai.AIRunParamsBodyTextEmbeddingsTextUnion](shared.UnionString("hello world")),
		},
	})
	if err != nil {
		log.Fatalf("Text embeddings: %v", err)
	}
	fmt.Printf("Text embeddings -> %+v\n", *embedResp)

	fmt.Printf("View trace: %s\n", bt.Permalink(rootSpan))
}
