package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	jq "github.com/itchyny/gojq"
)

const defaultJQTimeout = 500 * time.Millisecond

var (
	ErrJQSyntax      = fmt.Errorf("jq syntax error")
	ErrJQExecution   = fmt.Errorf("jq execution error")
	ErrJQTimeout     = fmt.Errorf("jq execution timeout")
	ErrJQEmptyInput  = fmt.Errorf("jq: empty workflow input")
	ErrJQInvalidJSON = fmt.Errorf("jq: invalid JSON input")
)

type jqRunResult struct {
	match bool
	err   error
}

func CompileJQExpression(expr string) (*jq.Query, error) {
	query, err := jq.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrJQSyntax, err.Error())
	}
	return query, nil
}

func ExecuteJQFilter(ctx context.Context, query *jq.Query, rawWorkflow string) (bool, error) {
	if strings.TrimSpace(rawWorkflow) == "" {
		return false, ErrJQEmptyInput
	}

	var input any
	if err := json.Unmarshal([]byte(rawWorkflow), &input); err != nil {
		return false, fmt.Errorf("%w: %s", ErrJQInvalidJSON, err.Error())
	}

	execCtx, cancel := context.WithTimeout(ctx, defaultJQTimeout)
	defer cancel()

	done := make(chan jqRunResult, 1)
	go func() {
		iter := query.Run(input)
		for {
			v, ok := iter.Next()
			if !ok {
				done <- jqRunResult{match: false}
				return
			}
			if err, isErr := v.(error); isErr {
				done <- jqRunResult{err: fmt.Errorf("%w: %s", ErrJQExecution, err.Error())}
				return
			}
			if isTruthy(v) {
				done <- jqRunResult{match: true}
				return
			}
		}
	}()

	select {
	case r := <-done:
		return r.match, r.err
	case <-execCtx.Done():
		return false, ErrJQTimeout
	}
}

func ExecuteJQFilters(ctx context.Context, expressions []string, rawWorkflow string) (bool, error) {
	if len(expressions) == 0 {
		return true, nil
	}

	queries := make([]*jq.Query, 0, len(expressions))
	for _, expr := range expressions {
		q, err := CompileJQExpression(expr)
		if err != nil {
			return false, err
		}
		queries = append(queries, q)
	}

	for _, q := range queries {
		match, err := ExecuteJQFilter(ctx, q, rawWorkflow)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}
	return true, nil
}

func isTruthy(v any) bool {
	if v == nil {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return val != ""
	case float64:
		return val != 0
	}
	return true
}

// EvalJQExpr evaluates an AST expression against a workflow JSON.
// It recursively handles AND/OR/NOT logic for jq filters.
// For non-jq expressions (TagExpr, regular FilterExpr), it returns true
// because those have already been filtered at the SQL layer.
func EvalJQExpr(ctx context.Context, expr Expr, rawWorkflow string) (bool, error) {
	if expr == nil {
		return true, nil
	}

	switch e := expr.(type) {
	case FilterExpr:
		if !e.IsJQFilter || e.JQExpr == "" {
			if e.Key == "comfyui" {
				return e.Val != "" && strings.Contains(strings.ToLower(rawWorkflow), strings.ToLower(e.Val)), nil
			}
			return true, nil
		}
		// jq filter: execute and return result
		query, err := CompileJQExpression(e.JQExpr)
		if err != nil {
			// Compile error: user input issue, report to frontend
			return false, fmt.Errorf("jq syntax error in %q: %w", e.JQExpr, err)
		}
		match, err := ExecuteJQFilter(ctx, query, rawWorkflow)
		if err != nil {
			// Execute error: missing workflow, timeout, etc. -> treat as no match
			return false, nil
		}
		return match, nil

	case TagExpr:
		// Tags are handled by SQL, return true
		return true, nil

	case AndExpr:
		// AND: both sides must match
		left, err := EvalJQExpr(ctx, e.Left, rawWorkflow)
		if err != nil {
			return false, err
		}
		if !left {
			return false, nil
		}
		return EvalJQExpr(ctx, e.Right, rawWorkflow)

	case OrExpr:
		// OR: either side must match
		left, err := EvalJQExpr(ctx, e.Left, rawWorkflow)
		if err != nil {
			return false, err
		}
		if left {
			return true, nil
		}
		return EvalJQExpr(ctx, e.Right, rawWorkflow)

	case NotExpr:
		// NOT: negate the result
		child, err := EvalJQExpr(ctx, e.Expr, rawWorkflow)
		if err != nil {
			return false, err
		}
		return !child, nil

	default:
		return true, nil
	}
}
