package evalrunner

import (
	"bufio"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/braintrustdata/braintrust-sdk-go/logger"
)

func TestSSESink_FrameFormat(t *testing.T) {
	var buf strings.Builder
	s := newSSESink(&buf, nil)

	require.NoError(t, s.send("summary", map[string]any{"a": 1}))

	assert.Equal(t, "event: summary\ndata: {\"a\":1}\n\n", buf.String())
}

func TestSSESink_StringDataPassesThroughUnmarshalled(t *testing.T) {
	var buf strings.Builder
	s := newSSESink(&buf, nil)

	require.NoError(t, s.send("progress", `{"already":"json"}`))

	assert.Equal(t, "event: progress\ndata: {\"already\":\"json\"}\n\n", buf.String())
}

// SSE forbids bare newlines inside a data field: each line needs its own
// "data: " prefix or bt's parser joins them wrongly.
func TestSSESink_MultilineDataIsPrefixedPerLine(t *testing.T) {
	var buf strings.Builder
	s := newSSESink(&buf, nil)

	require.NoError(t, s.send("console", "line one\nline two"))

	assert.Equal(t, "event: console\ndata: line one\ndata: line two\n\n", buf.String())
}

func TestSSESink_FlushesEveryFrame(t *testing.T) {
	var buf strings.Builder
	var flushes int
	s := newSSESink(&buf, func() { flushes++ })

	require.NoError(t, s.send("start", map[string]any{}))
	require.NoError(t, s.send("done", map[string]any{}))

	assert.Equal(t, 2, flushes)
}

func TestSSESink_ConcurrentSendsDoNotInterleave(t *testing.T) {
	var buf strings.Builder
	s := newSSESink(&buf, nil)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.send("progress", map[string]any{"n": 1})
		}()
	}
	wg.Wait()

	// Every frame must be intact: 50 well-formed frames, nothing spliced.
	assert.Equal(t, 50, strings.Count(buf.String(), "event: progress\ndata: {\"n\":1}\n\n"))
}

func TestDiscardSink_SwallowsEverything(t *testing.T) {
	s := discardSink{}

	assert.NoError(t, s.send("summary", make(chan int))) // unmarshallable on purpose
	assert.NoError(t, s.Close())
}

func TestDialSink_ConnectsToUnixSocket(t *testing.T) {
	sockPath := filepath.Join(socketDir(t), "sse.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	frames := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		line, _ := r.ReadString('\n')
		frames <- line
	}()

	s := dialSink(env{SSESock: sockPath}, logger.Discard())
	defer func() { _ = s.Close() }()

	require.IsType(t, &sseSink{}, s)
	require.NoError(t, s.send("start", map[string]any{}))

	assert.Equal(t, "event: start\n", <-frames)
}

func TestDialSink_FallsBackToTCPAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()

	frames := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		line, _ := r.ReadString('\n')
		frames <- line
	}()

	s := dialSink(env{SSEAddr: ln.Addr().String()}, logger.Discard())
	defer func() { _ = s.Close() }()

	require.IsType(t, &sseSink{}, s)
	require.NoError(t, s.send("start", map[string]any{}))

	assert.Equal(t, "event: start\n", <-frames)
}

// A missing or dead socket must never abort the eval: bt may not be listening,
// or we may be running standalone. Degrade to discard, never error.
func TestDialSink_MissingSocketDegradesToDiscard(t *testing.T) {
	s := dialSink(env{SSESock: filepath.Join(socketDir(t), "nope.sock")}, logger.Discard())

	assert.IsType(t, discardSink{}, s)
	assert.NoError(t, s.send("summary", map[string]any{}))
	assert.NoError(t, s.Close())
}

func TestDialSink_NoSocketConfiguredDegradesToDiscard(t *testing.T) {
	s := dialSink(env{}, logger.Discard())

	assert.IsType(t, discardSink{}, s)
	assert.NoError(t, s.Close())
}
