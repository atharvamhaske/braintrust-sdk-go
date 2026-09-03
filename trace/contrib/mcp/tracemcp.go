// Package tracemcp provides OpenTelemetry tracing for Model Context Protocol (MCP)
// clients and servers using the official Go SDK.
//
// First, set up tracing with braintrust.New():
//
//	tp := trace.NewTracerProvider()
//	defer tp.Shutdown(context.Background())
//	otel.SetTracerProvider(tp)
//
//	bt, err := braintrust.New(tp, braintrust.WithProject("my-project"))
//	if err != nil {
//		log.Fatal(err)
//	}
//
// Then instrument your MCP client and/or server:
//
//	client := mcp.NewClient(&mcp.Implementation{Name: "my-client"}, nil)
//	tracemcp.InstrumentClient(client)
//
//	server := mcp.NewServer(&mcp.Implementation{Name: "my-server"}, nil)
//	tracemcp.InstrumentServer(server)
package tracemcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/braintrustdata/braintrust-sdk-go/trace/internal"
)

const (
	methodCallTool  = "tools/call"
	methodListTools = "tools/list"
)

type side int

const (
	sideClient side = iota
	sideServer
)

type config struct {
	tracer trace.Tracer
}

// Option configures MCP tracing middleware.
type Option func(*config)

// WithTracerProvider sets a custom TracerProvider for MCP tracing.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(cfg *config) {
		cfg.tracer = tp.Tracer("braintrust")
	}
}

func newConfig(opts ...Option) *config {
	cfg := &config{
		tracer: otel.GetTracerProvider().Tracer("braintrust"),
	}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}

// InstrumentClient adds Braintrust tracing middleware to an MCP client.
// It traces client-side ListTools and CallTool requests.
func InstrumentClient(c *mcp.Client, opts ...Option) {
	if c == nil {
		return
	}
	cfg := newConfig(opts...)
	c.AddSendingMiddleware(tracingMiddleware(cfg, sideClient))
}

// InstrumentServer adds Braintrust tracing middleware to an MCP server.
// It traces server-side tools/list and tools/call handling, including tool handlers.
func InstrumentServer(s *mcp.Server, opts ...Option) {
	if s == nil {
		return
	}
	cfg := newConfig(opts...)
	s.AddReceivingMiddleware(tracingMiddleware(cfg, sideServer))
}

func tracingMiddleware(cfg *config, from side) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case methodCallTool, methodListTools:
			default:
				return next(ctx, method, req)
			}

			spanName := spanName(method, req)
			spanKind := trace.SpanKindClient
			if from == sideServer {
				spanKind = trace.SpanKindInternal
			}

			ctx, span := cfg.tracer.Start(ctx, spanName, trace.WithSpanKind(spanKind))
			defer span.End()

			setSpanType(span, method)
			setMetadata(span, method, req, from)
			setInput(span, method, req)

			result, err := next(ctx, method, req)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				return result, err
			}

			setOutput(span, method, result)
			return result, err
		}
	}
}

func spanName(method string, req mcp.Request) string {
	switch method {
	case methodCallTool:
		if name := callToolName(req.GetParams()); name != "" {
			return fmt.Sprintf("mcp.tools.call [%s]", name)
		}
		return "mcp.tools.call"
	case methodListTools:
		return "mcp.tools.list"
	default:
		return method
	}
}

func setSpanType(span trace.Span, method string) {
	spanType := "task"
	if method == methodCallTool {
		spanType = "tool"
	}
	_ = internal.SetJSONAttr(span, "braintrust.span_attributes", map[string]string{
		"type": spanType,
	})
}

func setMetadata(span trace.Span, method string, req mcp.Request, from side) {
	metadata := map[string]any{
		"provider": "mcp",
		"method":   method,
	}
	if from == sideClient {
		metadata["role"] = "client"
	} else {
		metadata["role"] = "server"
	}
	if sessionID := req.GetSession().ID(); sessionID != "" {
		metadata["session_id"] = sessionID
	}
	if method == methodCallTool {
		if name := callToolName(req.GetParams()); name != "" {
			metadata["name"] = name
		}
	}
	_ = internal.SetJSONAttr(span, "braintrust.metadata", metadata)
}

func setInput(span trace.Span, method string, req mcp.Request) {
	switch method {
	case methodCallTool:
		if input := callToolInput(req.GetParams()); input != nil {
			_ = internal.SetJSONAttr(span, "braintrust.input_json", input)
		}
	case methodListTools:
		params, ok := req.GetParams().(*mcp.ListToolsParams)
		if !ok || params == nil {
			return
		}
		input := map[string]any{}
		if params.Cursor != "" {
			input["cursor"] = params.Cursor
		}
		_ = internal.SetJSONAttr(span, "braintrust.input_json", input)
	}
}

func setOutput(span trace.Span, method string, result mcp.Result) {
	switch method {
	case methodCallTool:
		res, ok := result.(*mcp.CallToolResult)
		if !ok || res == nil {
			return
		}
		_ = internal.SetJSONAttr(span, "braintrust.output_json", callToolOutput(res))
	case methodListTools:
		res, ok := result.(*mcp.ListToolsResult)
		if !ok || res == nil {
			return
		}
		_ = internal.SetJSONAttr(span, "braintrust.output_json", listToolsOutput(res))
	}
}

func callToolName(params mcp.Params) string {
	switch p := params.(type) {
	case *mcp.CallToolParams:
		if p != nil {
			return p.Name
		}
	case *mcp.CallToolParamsRaw:
		if p != nil {
			return p.Name
		}
	}
	return ""
}

func callToolInput(params mcp.Params) map[string]any {
	switch p := params.(type) {
	case *mcp.CallToolParams:
		if p == nil {
			return nil
		}
		input := map[string]any{"name": p.Name}
		if p.Arguments != nil {
			input["arguments"] = p.Arguments
		}
		return input
	case *mcp.CallToolParamsRaw:
		if p == nil {
			return nil
		}
		input := map[string]any{"name": p.Name}
		if len(p.Arguments) > 0 {
			var args any
			if err := json.Unmarshal(p.Arguments, &args); err == nil {
				input["arguments"] = args
			} else {
				input["arguments"] = json.RawMessage(p.Arguments)
			}
		}
		return input
	default:
		return nil
	}
}

func callToolOutput(result *mcp.CallToolResult) map[string]any {
	output := map[string]any{}
	if result.IsError {
		output["is_error"] = true
	}
	if result.StructuredContent != nil {
		output["structured_content"] = result.StructuredContent
	}
	if texts := contentTexts(result.Content); len(texts) == 1 {
		output["content"] = texts[0]
	} else if len(texts) > 1 {
		output["content"] = texts
	}
	if len(output) == 0 {
		return nil
	}
	return output
}

func listToolsOutput(result *mcp.ListToolsResult) map[string]any {
	tools := make([]map[string]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		if tool == nil {
			continue
		}
		entry := map[string]string{"name": tool.Name}
		if tool.Description != "" {
			entry["description"] = tool.Description
		}
		tools = append(tools, entry)
	}
	output := map[string]any{
		"tools": tools,
		"count": len(tools),
	}
	if result.NextCursor != "" {
		output["next_cursor"] = result.NextCursor
	}
	return output
}

func contentTexts(content []mcp.Content) []string {
	texts := make([]string, 0, len(content))
	for _, item := range content {
		if text, ok := item.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	return texts
}
