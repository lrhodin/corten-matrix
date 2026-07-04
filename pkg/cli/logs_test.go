package cli

import (
	"strings"
	"testing"
)

func TestPrettyTail(t *testing.T) {
	in := strings.NewReader(
		`{"level":"info","time":"2026-06-27T23:07:08.772457132-04:00","message":"Initializing bridge"}` + "\n" +
			`{"level":"debug","db_section":"main","time":"2026-06-27T23:07:08.78-04:00","message":"Database is up to date"}` + "\n" +
			"plain non-json line\n" +
			"\x1b[32mold colored pretty line\x1b[0m\n" +
			"{not valid json}\n")
	var out strings.Builder
	prettyTail(in, &out, true)
	got := out.String()

	for _, want := range []string{
		"INF", "Initializing bridge", // json rendered as pretty console line
		"DBG", "Database is up to date", "db_section=main",
		"plain non-json line",             // passthrough
		"\x1b[32mold colored pretty line", // passthrough, untouched
		"{not valid json}",                // invalid json falls back to passthrough
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"level"`) {
		t.Errorf("json line not prettified:\n%s", got)
	}
	if lines, want := strings.Count(got, "\n"), 5; lines != want {
		t.Errorf("got %d lines, want %d:\n%s", lines, want, got)
	}
}
