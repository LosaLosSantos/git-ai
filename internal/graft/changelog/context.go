// Package changelog records every reference update accepted by the server in
// an append-only, per-repository changeset log, and serves it to the
// ChangesetService.
//
// This file holds the context helpers that writers use to annotate the
// updates they perform; the log itself lives in the rest of the package.
package changelog

import (
	"context"
	"strings"
	"unicode"

	"google.golang.org/grpc"
)

type rewrittenKey struct{}

type operationKey struct{}

type batchKey struct{}

// WithBatch records the changes that a batched land folds into the reference
// updates performed with ctx. The updates then carry them as Entry.Members.
func WithBatch(ctx context.Context, members []Member) context.Context {
	return context.WithValue(ctx, batchKey{}, members)
}

// BatchFromContext returns the members set by WithBatch, or nil when there are
// none.
func BatchFromContext(ctx context.Context) []Member {
	members, _ := ctx.Value(batchKey{}).([]Member)
	return members
}

// WithRewritten marks reference updates performed with ctx as having
// rewritten history (e.g. a land that rebased the change onto a newer tip).
func WithRewritten(ctx context.Context) context.Context {
	return context.WithValue(ctx, rewrittenKey{}, true)
}

// IsRewritten reports whether ctx was marked with WithRewritten.
func IsRewritten(ctx context.Context) bool {
	v, _ := ctx.Value(rewrittenKey{}).(bool)
	return v
}

// WithOperation sets the operation name recorded for reference updates
// performed with ctx, overriding the name derived from the gRPC method.
func WithOperation(ctx context.Context, operation string) context.Context {
	return context.WithValue(ctx, operationKey{}, operation)
}

// OperationFromContext returns the operation name for reference updates
// performed with ctx: the value set by WithOperation, otherwise the gRPC
// method name converted to snake case without a leading "User" (so
// "/gitaly.OperationService/UserFFBranch" becomes "ff_branch" and
// "/gitaly.OperationService/UserLand" becomes "land"), otherwise "unknown".
func OperationFromContext(ctx context.Context) string {
	if op, ok := ctx.Value(operationKey{}).(string); ok && op != "" {
		return op
	}
	method, ok := grpc.Method(ctx)
	if !ok || method == "" {
		return "unknown"
	}
	return operationFromMethod(method)
}

func operationFromMethod(fullMethod string) string {
	name := fullMethod
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if strings.HasPrefix(name, "User") && len(name) > len("User") {
		name = name[len("User"):]
	}
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		if unicode.IsUpper(r) {
			// Start a new word before an upper-case rune that follows a
			// lower-case rune, or that starts a new word after an acronym
			// ("FFBranch" -> "ff_branch").
			if i > 0 && (unicode.IsLower(runes[i-1]) ||
				(i+1 < len(runes) && unicode.IsLower(runes[i+1]) && unicode.IsUpper(runes[i-1]))) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
