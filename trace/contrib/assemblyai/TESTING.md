# Testing the AssemblyAI LeMUR integration

Five tests need real recorded cassettes before this ships: `TestLeMURTask`,
`TestLeMURSummarize`, `TestLeMURActionItems`, `TestLeMURQuestion`, and
`TestLeMURTaskError`. All five currently fail with `cassette not found` —
that's expected, not a bug.

## 1. Get an API key

AssemblyAI gives every new account **$50 in free API credit, no credit card
required**. Sign up at [assemblyai.com](https://www.assemblyai.com), grab the
key from the dashboard. No separate model-access approval step (unlike
Bedrock) — the key works immediately.

## 2. Export it locally

```
export ASSEMBLYAI_API_KEY=<key>
```

Export it directly in your shell — never paste it into a chat session.

## 3. Record the cassettes

```
cd trace/contrib/assemblyai
VCR_MODE=record go test -v -run='TestLeMUR' ./...
```

Expected output — five `--- PASS:` lines:

```
=== RUN   TestLeMURTask
--- PASS: TestLeMURTask (...)
=== RUN   TestLeMURSummarize
--- PASS: TestLeMURSummarize (...)
=== RUN   TestLeMURActionItems
--- PASS: TestLeMURActionItems (...)
=== RUN   TestLeMURQuestion
--- PASS: TestLeMURQuestion (...)
=== RUN   TestLeMURTaskError
--- PASS: TestLeMURTaskError (...)
PASS
```

This writes five new files under `testdata/cassettes/`:
`TestLeMURTask.yaml`, `TestLeMURSummarize.yaml`, `TestLeMURActionItems.yaml`,
`TestLeMURQuestion.yaml`, `TestLeMURTaskError.yaml`.

## 4. Verify replay works offline

```
go test -v -run='TestLeMUR' ./...
```

(no `VCR_MODE` — defaults to replay.) Same five `PASS` lines, no network
calls, no API key needed from here on.

## 5. Full check

```
cd ../../..
make ci
```

Should be fully green.

## 6. Commit the cassettes

```
git add trace/contrib/assemblyai/testdata/cassettes/
git commit -sm "test: record assemblyai lemur cassettes"
```

## Cost note

Each test call is a short prompt (one paragraph of transcript, ~50 tokens of
output) — five recordings will use a negligible fraction of the $50 free
credit.
