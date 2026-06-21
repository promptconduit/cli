package eventlog

import (
	"fmt"
	"os"
	"strings"
)

// Errorf appends a human-readable error line to errors.log. Use it for any
// failure path that should be visible when debugging "events aren't reaching
// the platform" — failed sends, dropped events, etc.
//
// Line format mirrors internal/logger for familiarity:
//
//	<RFC3339> ERROR pid=<pid> <message>
//
// Best-effort and gated on Enabled(); never blocks the caller.
func Errorf(format string, args ...interface{}) {
	if !Enabled() {
		return
	}
	writeErrorLine("ERROR", fmt.Sprintf(format, args...))
}

// RecordDrop logs an event the CLI handled but did NOT send, with the reason
// (e.g. "not_configured", "parse_error", "empty_stdin") and a short context
// string. It both writes a line to errors.log and bumps the "dropped" counter,
// so silent drops become visible in `promptconduit status` and the log.
func RecordDrop(reason, context string) {
	if !Enabled() {
		return
	}
	detail := reason
	if context != "" {
		detail = reason + ": " + context
	}
	writeErrorLine("DROP", "event dropped ("+detail+")")
	Bump(OutcomeDropped, detail)
}

func writeErrorLine(level, msg string) {
	// Collapse newlines so each record stays a single grep-able line.
	msg = strings.ReplaceAll(msg, "\n", " ")
	line := fmt.Sprintf("%s %s pid=%d %s",
		nowUTC().Format(timeLayout),
		level,
		os.Getpid(),
		msg,
	)
	appendLine(ErrorsPath(), []byte(line), ErrorsRotateAt)
}

// TailErrors returns up to maxLines lines from the end of errors.log, or an
// empty string when the file doesn't exist yet.
func TailErrors(maxLines int) (string, error) {
	return tailFile(ErrorsPath(), maxLines)
}
