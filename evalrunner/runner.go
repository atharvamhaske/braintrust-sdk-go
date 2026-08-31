// Package evalrunner runs Braintrust evals under the `bt` CLI.
//
// A program built with this package is not a server. bt owns the HTTP port,
// authentication and CORS; it spawns this binary once per request, passes the
// request in environment variables, and reads results back over a unix socket.
// The process runs one eval and exits.
//
// A minimal runner:
//
//	func main() {
//	    r := evalrunner.New()
//
//	    evalrunner.RegisterEval(r, classify)
//
//	    evalrunner.Main(r)
//	}
//
// Then point bt at the package directory:
//
//	bt eval --dev --language go ./cmd/evals
//
// Decoding the request and dispatching to the right eval happens inside
// [Main]; registered evals are looked up by their [eval.Eval] Name.
package evalrunner

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/braintrustdata/braintrust-sdk-go/logger"
)

const defaultAppURL = "https://www.braintrust.dev"

// Runner holds the evals a program exposes to bt.
//
// Registration happens once, before [Run], from a single goroutine, so the
// evaluator map needs no locking.
type Runner struct {
	evaluators map[string]registeredEval
	// order preserves registration order for listings, so output is stable.
	order []string

	logger         logger.Logger
	tracerProvider *sdktrace.TracerProvider

	env env
	// stdout is where protocol output goes. Overridable for tests; nothing else
	// in the package may write to it, because bt parses the manifest from it.
	stdout io.Writer

	// allPassed tracks whether anything failed, for the exit code.
	allPassed bool

	// newSession builds the authenticated Braintrust context. Overridable so
	// tests can supply a VCR-backed session instead of hitting the network.
	newSession func(context.Context) (*session, error)
}

// Option configures a [Runner].
type Option func(*Runner)

// WithLogger sets a custom logger. Logs must go to stderr: bt reads the eval
// manifest from stdout.
func WithLogger(l logger.Logger) Option {
	return func(r *Runner) {
		r.logger = l
	}
}

// WithTracerProvider sets a custom OpenTelemetry TracerProvider.
//
// Supply one when instrumented code (LLM clients, custom spans) should appear
// in the same trace as eval spans. When nil, a provider is created per run and
// shut down before the process exits.
func WithTracerProvider(tp *sdktrace.TracerProvider) Option {
	return func(r *Runner) {
		r.tracerProvider = tp
	}
}

// New creates a runner.
func New(opts ...Option) *Runner {
	r := &Runner{
		evaluators: make(map[string]registeredEval),
		logger:     logger.NewDefaultLogger(),
		env:        readEnv(os.Getenv),
		stdout:     os.Stdout,
		allPassed:  true,
	}
	r.newSession = func(ctx context.Context) (*session, error) {
		return newSessionFromEnv(ctx, r.logger)
	}

	for _, opt := range opts {
		opt(r)
	}

	// Filters are parsed once the logger is known, so a malformed filter warns
	// somewhere visible instead of vanishing.
	r.env.Filters = parseFilters(os.Getenv("BT_EVAL_FILTER_PARSED"), r.logger)

	return r
}

// Mode reports what bt is asking this process to do. Use it to skip expensive
// setup when bt only wants the list of evals:
//
//	if r.Mode() != evalrunner.ModeList {
//	    warmCaches()
//	}
func (r *Runner) Mode() Mode {
	return r.env.Mode()
}

// Main runs the runner and exits the process. It never returns.
//
// It exists so the common case is one line. Programs that need to do their own
// cleanup should call [Run] instead and handle the error themselves.
func Main(r *Runner) {
	if err := Run(context.Background(), r); err != nil {
		r.logger.Error("eval runner failed", "error", err)
		os.Exit(1)
	}
	os.Exit(r.exitCode())
}

// Run dispatches on the environment bt provided and blocks until finished.
//
// os.Args is deliberately ignored: bt constructs argv for its own runner
// scripts and splices arguments in that mean nothing to us.
func Run(ctx context.Context, r *Runner) error {
	switch r.Mode() {
	case ModeList:
		return r.runList()
	case ModeEval:
		return r.runEval(ctx)
	case ModeBatch:
		return r.runBatch(ctx)
	case ModeInspect:
		return r.runInspect()
	case ModeUnknown:
		return fmt.Errorf("unrecognised BT_EVAL_DEV_MODE %q (expected \"list\" or \"eval\")", r.env.DevMode)
	default:
		return fmt.Errorf("unrecognised BT_EVAL_DEV_MODE %q (expected \"list\" or \"eval\")", r.env.DevMode)
	}
}

// exitCode applies the exit-code policy.
//
// Case failures must NOT exit non-zero when bt is streaming results: bt
// synthesises a spurious `error` frame for any child that exits non-zero
// without having sent one, and the playground renders that as a failed run even
// though a perfectly good summary already arrived (bt/src/eval.rs:1691). Batch
// runs have no playground watching, so there a non-zero exit is the useful
// signal for CI.
func (r *Runner) exitCode() int {
	if r.Mode() == ModeBatch && !r.allPassed {
		return 1
	}
	return 0
}

// runInspect prints what is registered and exits, without contacting
// Braintrust. This is what a bare `go run ./cmd/evals` does: checking that a
// program compiles and is wired up should not create experiments as a side
// effect.
func (r *Runner) runInspect() error {
	out := &strings.Builder{}
	if len(r.order) == 0 {
		out.WriteString("No evals registered.\n")
		out.WriteString("Register one with evalrunner.RegisterEval(r, myEval).\n")
		_, err := fmt.Fprint(r.stdout, out.String())
		return err
	}

	out.WriteString("Registered evals:\n")
	for _, name := range r.order {
		e := r.evaluators[name]
		out.WriteString("  " + name)

		var details []string
		if scorers := e.scorerNames(); len(scorers) > 0 {
			details = append(details, "scorers: "+strings.Join(scorers, ", "))
		}
		if schema := e.parameterSchema(); len(schema) > 0 {
			names := make([]string, 0, len(schema))
			for name := range schema {
				names = append(names, name)
			}
			sort.Strings(names)
			details = append(details, "parameters: "+strings.Join(names, ", "))
		}
		if len(details) > 0 {
			out.WriteString("  (" + strings.Join(details, "; ") + ")")
		}
		out.WriteString("\n")
	}
	out.WriteString("\nRun them with: bt eval <this package directory>\n")

	_, err := fmt.Fprint(r.stdout, out.String())
	return err
}
