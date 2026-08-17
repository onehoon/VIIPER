package main

import (
	"context"
	"log/slog"
	"sync"
)

type deferredLogBatch struct {
	mu      sync.Mutex
	records []slog.Record
	logger  *slog.Logger
}

type deferredLogHandler struct {
	batch  *deferredLogBatch
	attrs  []slog.Attr
	groups []string
}

func newDeferredLogBatch() *deferredLogBatch {
	b := &deferredLogBatch{}
	b.logger = slog.New(&deferredLogHandler{batch: b})
	return b
}

func (h *deferredLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *deferredLogHandler) Handle(_ context.Context, record slog.Record) error {
	clone := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	attrs := append([]slog.Attr(nil), h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})
	if len(h.groups) > 0 {
		group := slog.Group(h.groups[len(h.groups)-1], attrsToAny(attrs)...)
		for i := len(h.groups) - 2; i >= 0; i-- {
			group = slog.Group(h.groups[i], group)
		}
		clone.AddAttrs(group)
	} else {
		clone.AddAttrs(attrs...)
	}
	h.batch.mu.Lock()
	h.batch.records = append(h.batch.records, clone.Clone())
	h.batch.mu.Unlock()
	return nil
}

func attrsToAny(attrs []slog.Attr) []any {
	values := make([]any, len(attrs))
	for i, attr := range attrs {
		values[i] = attr
	}
	return values
}

func (h *deferredLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *deferredLogHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

func (b *deferredLogBatch) replay(logger *slog.Logger) {
	b.mu.Lock()
	records := append([]slog.Record(nil), b.records...)
	b.mu.Unlock()
	for _, record := range records {
		_ = logger.Handler().Handle(context.Background(), record)
	}
}
