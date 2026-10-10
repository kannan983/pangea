// Package logx sets up Pangea's log file (slog, text format) so a person or
// an agent can debug a session afterwards.
//
//	PANGEA_LOG=debug|info|warn|error   (default info)
//	file: <config dir>/pangea.log, rotated to pangea.log.1 at 5 MB
//
// Never log secrets: passwords, passphrases, prompt answers or key contents.
package logx

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

const maxSize = 5 << 20

// Init points slog's default logger at dir/pangea.log and returns the file
// to close on exit, and the log path.
func Init(dir string) (io.Closer, string, error) {
	p := filepath.Join(dir, "pangea.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxSize {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: Level(os.Getenv("PANGEA_LOG"))})))
	return f, p, nil
}

// Level parses a PANGEA_LOG value; unknown values mean info.
func Level(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
