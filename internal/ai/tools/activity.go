package tools

import "context"

type activityKey struct{}

func WithActivity(ctx context.Context, observer func(string, string)) context.Context {
	return context.WithValue(ctx, activityKey{}, observer)
}

func ReportActivity(ctx context.Context, state, detail string) {
	if observer, ok := ctx.Value(activityKey{}).(func(string, string)); ok && observer != nil {
		observer(state, detail)
	}
}
