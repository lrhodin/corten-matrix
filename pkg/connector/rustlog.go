// corten-matrix - A Matrix-iMessage puppeting bridge.

package connector

import (
	"github.com/rs/zerolog"

	"github.com/lrhodin/corten-matrix/pkg/rustpushgo"
)

// rustLogSink forwards the Rust wrapper's log records into the bridge logger,
// so rustpushgo and rustpush lines land in bridge.log next to the Go lines
// and `corten-matrix logs` shows them. Before this they went to the process's
// stderr, which only the per-account stdout capture file received. Levels map
// one to one; the Rust module path becomes the "target" field.
type rustLogSink struct {
	log zerolog.Logger
}

func (s *rustLogSink) Log(level string, target string, message string) {
	s.log.WithLevel(rustLogLevel(level)).Str("target", target).Msg(message)
}

func rustLogLevel(level string) zerolog.Level {
	switch level {
	case "ERROR":
		return zerolog.ErrorLevel
	case "WARN":
		return zerolog.WarnLevel
	case "INFO":
		return zerolog.InfoLevel
	case "DEBUG":
		return zerolog.DebugLevel
	case "TRACE":
		return zerolog.TraceLevel
	default:
		return zerolog.InfoLevel
	}
}

// installRustLogSink routes Rust logging into log. It must run before any
// other rustpushgo call in this process: a Rust logger can only be installed
// once, and the plain InitLogger fallback would otherwise win and keep the
// Rust lines out of the bridge log.
func installRustLogSink(log zerolog.Logger) {
	rustpushgo.InitLoggerWithSink(&rustLogSink{
		log: log.With().Str("component", "rustpush").Logger(),
	})
}
