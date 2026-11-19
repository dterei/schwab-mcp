package mcpserver

import (
	"context"
	"log/slog"
)

// The server logs through one handler that rewrites every line before the
// handler it was given writes it (design 5.4): each attribute's value, the
// message, and the attributes given to With, groups' members included, are
// turned into text by masker.logText: an error's text masked as free text,
// the known numbers, the app key and secret, the tokens and the home
// directory replaced. The SDK's own lines pass through it too, so that an
// error the SDK logs whole is masked as well. Lines are composed from
// masked pieces; no pattern runs over the server's own words but an
// error's.

// logHandler masks what inner writes.
type logHandler struct {
	inner slog.Handler
	m     *masker
}

func (h *logHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *logHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, h.m.logString(r.Message, false), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.m.logText(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	masked := make([]slog.Attr, len(as))
	for i, a := range as {
		masked[i] = h.m.logText(a)
	}
	return &logHandler{inner: h.inner.WithAttrs(masked), m: h.m}
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	return &logHandler{inner: h.inner.WithGroup(name), m: h.m}
}

// atLeast passes on only the lines of inner's at least as severe as min:
// the SDK's logger, which logs at warn and above (design 4.3), so that its
// lifecycle lines stay out of an info log.
type atLeast struct {
	slog.Handler
	min slog.Level
}

func (h atLeast) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.min && h.Handler.Enabled(ctx, level)
}

func (h atLeast) WithAttrs(as []slog.Attr) slog.Handler {
	return atLeast{Handler: h.Handler.WithAttrs(as), min: h.min}
}

func (h atLeast) WithGroup(name string) slog.Handler {
	return atLeast{Handler: h.Handler.WithGroup(name), min: h.min}
}
