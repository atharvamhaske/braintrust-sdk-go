package evalrunner

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/braintrustdata/braintrust-sdk-go/logger"
)

// sink is where protocol events go.
//
// bt spawns this process per request and reads Server-Sent Events back over a
// unix socket whose path arrives in BT_EVAL_SSE_SOCK. Keeping that behind an
// interface keeps the eval-running core free of transport details, so the
// long-lived/proxy shape discussed for other languages can be added later
// without touching register.go.
type sink interface {
	send(event string, data any) error
	io.Closer
}

// sseSink formats Server-Sent Events onto a writer.
//
// Safe for concurrent use: eval cases can run in parallel and each completion
// emits frames, so writes must not interleave mid-frame.
type sseSink struct {
	mu     sync.Mutex
	w      io.Writer
	flush  func()
	closer io.Closer
}

// newSSESink writes frames to w. flush, when non-nil, is called after every
// frame; a net.Conn needs no flushing, but a buffered writer does.
func newSSESink(w io.Writer, flush func()) *sseSink {
	closer, _ := w.(io.Closer)
	return &sseSink{w: w, flush: flush, closer: closer}
}

func (s *sseSink) send(event string, data any) error {
	// Marshal outside the lock: it is the expensive part and needs no ordering.
	payload, err := encodeSSEData(data)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return fmt.Errorf("failed to write SSE event %q: %w", event, err)
	}
	if s.flush != nil {
		s.flush()
	}
	return nil
}

// Close releases the underlying connection. This matters: bt waits for our end
// of the socket to close before it finishes the request, so leaking the
// connection hangs bt indefinitely -- there are no timeouts on its side.
func (s *sseSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closer == nil {
		return nil
	}
	return s.closer.Close()
}

// encodeSSEData renders an event payload. Strings and byte slices are assumed
// to be pre-encoded and pass through untouched.
func encodeSSEData(data any) (string, error) {
	var payload string
	switch v := data.(type) {
	case string:
		payload = v
	case []byte:
		payload = string(v)
	default:
		encoded, err := json.Marshal(data)
		if err != nil {
			return "", fmt.Errorf("failed to marshal SSE data: %w", err)
		}
		payload = string(encoded)
	}

	// An SSE data field cannot contain bare newlines: every line needs its own
	// "data: " prefix or the receiver joins them into one mangled value.
	return strings.ReplaceAll(payload, "\n", "\ndata: "), nil
}

// discardSink drops every event. Used when nothing is listening -- a batch run,
// or someone running the binary directly.
type discardSink struct{}

func (discardSink) send(string, any) error { return nil }
func (discardSink) Close() error           { return nil }

// dialSink connects to whichever event channel bt provided.
//
// Failure is deliberately never fatal. A missing or dead socket means nobody is
// listening, which is entirely normal outside the dev server, so we log and
// carry on emitting into the void rather than abandoning the eval.
func dialSink(e env, log logger.Logger) sink {
	if e.SSESock != "" {
		conn, err := net.Dial("unix", e.SSESock)
		if err == nil {
			return newSSESink(conn, nil)
		}
		logWarn(log, "could not connect to eval SSE socket; continuing without streaming",
			"path", e.SSESock, "error", err)
	}

	if e.SSEAddr != "" {
		conn, err := net.Dial("tcp", e.SSEAddr)
		if err == nil {
			return newSSESink(conn, nil)
		}
		logWarn(log, "could not connect to eval SSE address; continuing without streaming",
			"addr", e.SSEAddr, "error", err)
	}

	return discardSink{}
}
