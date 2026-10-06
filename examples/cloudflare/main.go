// This example demonstrates basic Cloudflare Workers AI tracing with Braintrust.
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
	ctx, span := otel.Tracer("cloudflare-example").Start(context.Background(), "examples/cloudflare/main.go")
	defer span.End()

	response, err := client.AI.Run(ctx, "@cf/meta/llama-3.1-8b-instruct-fast", ai.AIRunParams{
		AccountID: cf.F(os.Getenv("CLOUDFLARE_ACCOUNT_ID")),
		Body: ai.AIRunParamsBodyTextGeneration{
			Messages: cf.F([]ai.AIRunParamsBodyTextGenerationMessage{{
				Role:    cf.F("user"),
				Content: cf.F[ai.AIRunParamsBodyTextGenerationMessagesContentUnion](shared.UnionString("Say hello")),
			}}),
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Response: %+v\n", *response)
	fmt.Printf("View trace: %s\n", bt.Permalink(span))
}
