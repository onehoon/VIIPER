package main

import (
	"strings"
	"testing"
)

func TestFormatDocCommentLine_Empty(t *testing.T) {
	got := formatDocCommentLine("")
	if got != " *" {
		t.Fatalf("formatDocCommentLine(\"\") = %q, want %q", got, " *")
	}
	if strings.HasSuffix(got, " ") || strings.HasSuffix(got, "\t") {
		t.Fatalf("formatDocCommentLine(\"\") = %q has trailing whitespace", got)
	}
}

func TestFormatDocCommentLine_NonEmpty(t *testing.T) {
	got := formatDocCommentLine("Example text")
	if got != " * Example text" {
		t.Fatalf("formatDocCommentLine(\"Example text\") = %q, want %q", got, " * Example text")
	}
}

func TestFormatDocCommentLine_BlockHasNoTrailingWhitespace(t *testing.T) {
	doc := "Summary line.\n\nSecond paragraph.\n"
	var out []string
	for _, dl := range strings.Split(doc, "\n") {
		out = append(out, formatDocCommentLine(dl))
	}
	for i, line := range out {
		if strings.HasSuffix(line, " ") || strings.HasSuffix(line, "\t") {
			t.Fatalf("line %d %q has trailing whitespace", i, line)
		}
	}
}
