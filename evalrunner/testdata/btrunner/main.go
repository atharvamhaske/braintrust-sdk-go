// Command btrunner is a fixture for the subprocess tests in the evalrunner
// package. It is a realistic runner: register an eval, hand over to Main.
//
// It lives under testdata so the go tool does not pick it up in ./... builds;
// the tests build it by explicit path.
package main

import (
	"context"

	"github.com/braintrustdata/braintrust-sdk-go/eval"
	"github.com/braintrustdata/braintrust-sdk-go/evalrunner"
)

func main() {
	r := evalrunner.New()

	evalrunner.RegisterEval(r, &eval.Eval[string, string]{
		Name:        "food-classifier",
		ProjectName: "go-sdk-tests",
		ParameterSchema: eval.ParameterSchema{
			"model": {Type: "model", Default: "rule-based"},
		},
		Task: eval.T(func(_ context.Context, input string) (string, error) {
			if input == "apple" {
				return "fruit", nil
			}
			return "unknown", nil
		}),
		Scorers: []eval.Scorer[string, string]{
			eval.NewScorer("exact_match", func(_ context.Context, res eval.TaskResult[string, string]) (eval.Scores, error) {
				if res.Output == res.Expected {
					return eval.S(1.0), nil
				}
				return eval.S(0.0), nil
			}),
		},
	})

	evalrunner.Main(r)
}
