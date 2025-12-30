package app

import (
	"context"
	"strings"
)

type taskIDContextKey struct{}

func WithTaskID(ctx context.Context, taskID string) context.Context {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return ctx
	}
	return context.WithValue(ctx, taskIDContextKey{}, taskID)
}

func TaskIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, ok := ctx.Value(taskIDContextKey{}).(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
