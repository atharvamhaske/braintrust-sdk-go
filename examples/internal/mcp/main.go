package main

import (
	"context"
	"fmt"
	"log"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"

	"github.com/braintrustdata/braintrust-sdk-go"
	tracemcp "github.com/braintrustdata/braintrust-sdk-go/trace/contrib/mcp"
)

type greetArgs struct {
	Name string `json:"name" jsonschema:"the person to greet"`
}

type addArgs struct {
	A int `json:"a"`
	B int `json:"b"`
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

	tracer := otel.Tracer("mcp-internal-example")
	ctx, span := tracer.Start(context.Background(), "examples/internal/mcp/main.go")
	defer span.End()

	server := mcp.NewServer(&mcp.Implementation{Name: "kitchensink-server", Version: "v1.0.0"}, nil)
	tracemcp.InstrumentServer(server)

	mcp.AddTool(server, &mcp.Tool{Name: "greet", Description: "say hi"},
		func(_ context.Context, _ *mcp.CallToolRequest, args greetArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Hi " + args.Name}},
			}, nil, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "add", Description: "add two numbers"},
		func(_ context.Context, _ *mcp.CallToolRequest, args addArgs) (*mcp.CallToolResult, int, error) {
			return nil, args.A + args.B, nil
		})

	client := mcp.NewClient(&mcp.Implementation{Name: "kitchensink-client", Version: "v1.0.0"}, nil)
	tracemcp.InstrumentClient(client)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		log.Fatalf("server connect: %v", err)
	}
	defer func() { _ = serverSession.Wait() }()

	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		log.Fatalf("client connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		log.Fatalf("ListTools: %v", err)
	}
	fmt.Printf("ListTools returned %d tool(s):", len(tools.Tools))
	for _, tool := range tools.Tools {
		fmt.Printf(" %s", tool.Name)
	}
	fmt.Println()

	greetResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "greet",
		Arguments: greetArgs{Name: "Braintrust"},
	})
	if err != nil {
		log.Fatalf("CallTool greet: %v", err)
	}
	if text, ok := greetResult.Content[0].(*mcp.TextContent); ok {
		fmt.Printf("greet -> %s\n", text.Text)
	}

	addResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "add",
		Arguments: addArgs{A: 2, B: 3},
	})
	if err != nil {
		log.Fatalf("CallTool add: %v", err)
	}
	if text, ok := addResult.Content[0].(*mcp.TextContent); ok {
		fmt.Printf("add -> %s\n", text.Text)
	}

	fmt.Printf("View trace: %s\n", bt.Permalink(span))
}
