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
	sets   []deferredAttrSet
	groups []string
}

type deferredAttrSet struct {
	groups []string
	attrs  []slog.Attr
}

type deferredAttrNode struct {
	entries  []deferredAttrEntry
	children map[string]*deferredAttrNode
}

type deferredAttrEntry struct {
	attr  *slog.Attr
	group string
}

func newDeferredLogBatch() *deferredLogBatch {
	b := &deferredLogBatch{}
	b.logger = slog.New(&deferredLogHandler{batch: b})
	return b
}

func (h *deferredLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *deferredLogHandler) Handle(_ context.Context, record slog.Record) error {
	clone := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	root := &deferredAttrNode{}
	for _, set := range h.sets {
		addDeferredAttrs(root, set.groups, set.attrs)
	}
	var recordAttrs []slog.Attr
	record.Attrs(func(attr slog.Attr) bool {
		recordAttrs = append(recordAttrs, attr)
		return true
	})
	addDeferredAttrs(root, h.groups, recordAttrs)
	clone.AddAttrs(deferredNodeAttrs(root)...)
	h.batch.mu.Lock()
	h.batch.records = append(h.batch.records, clone.Clone())
	h.batch.mu.Unlock()
	return nil
}

func addDeferredAttrs(root *deferredAttrNode, groups []string, attrs []slog.Attr) {
	node := root
	for _, name := range groups {
		if name == "" {
			continue
		}
		if node.children == nil {
			node.children = make(map[string]*deferredAttrNode)
		}
		child := node.children[name]
		if child == nil {
			child = &deferredAttrNode{}
			node.children[name] = child
			node.entries = append(node.entries, deferredAttrEntry{group: name})
		}
		node = child
	}
	node.entries = append(node.entries, attrsToDeferredEntries(attrs)...)
}

func attrsToDeferredEntries(attrs []slog.Attr) []deferredAttrEntry {
	entries := make([]deferredAttrEntry, 0, len(attrs))
	for i := range attrs {
		attr := attrs[i]
		entries = append(entries, deferredAttrEntry{attr: &attr})
	}
	return entries
}

func deferredNodeAttrs(node *deferredAttrNode) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(node.entries))
	for _, entry := range node.entries {
		if entry.attr != nil {
			attrs = append(attrs, *entry.attr)
			continue
		}
		attrs = append(attrs, slog.Group(entry.group, attrsToAny(deferredNodeAttrs(node.children[entry.group]))...))
	}
	return attrs
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
	clone.sets = append(append([]deferredAttrSet(nil), h.sets...), deferredAttrSet{
		groups: append([]string(nil), h.groups...),
		attrs:  append([]slog.Attr(nil), attrs...),
	})
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
		if logger.Enabled(context.Background(), record.Level) {
			_ = logger.Handler().Handle(context.Background(), record)
		}
	}
}
