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

// Catalog source context: tracks which catalog item an app was installed from.
// Used for icon lookup and update tracking.
type catalogSourceContextKey struct{}

func WithCatalogSource(ctx context.Context, catalogSource string) context.Context {
	catalogSource = strings.TrimSpace(catalogSource)
	if catalogSource == "" {
		return ctx
	}
	return context.WithValue(ctx, catalogSourceContextKey{}, catalogSource)
}

func CatalogSourceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, ok := ctx.Value(catalogSourceContextKey{}).(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
