package persist

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gstop/internal/logging"
	"gstop/internal/model"
)

// whitespace collapses runs of spaces in the version string into underscores.
var whitespace = regexp.MustCompile(`\s+`)

// rotatingWriter appends lines to a data-log file, rolling over by size: the full
// file is gzip-compressed and a new file (named from the current DBInfo and a
// fresh timestamp) is opened. Port of CompressedDynamicFileHandler.
type rotatingWriter struct {
	dbInfo      *model.DBInfo
	baseDir     string
	maxBytes    int64
	backupCount int
	logger      *logging.Logger

	file    *os.File
	name    string
	size    int64
	nowFunc func() time.Time
}

// newRotatingWriter creates the log directory and opens the first file.
func newRotatingWriter(dbInfo *model.DBInfo, baseDir string, maxBytes int64, backupCount int, logger *logging.Logger) *rotatingWriter {
	w := &rotatingWriter{
		dbInfo:      dbInfo,
		baseDir:     baseDir,
		maxBytes:    maxBytes,
		backupCount: backupCount,
		logger:      logger,
		nowFunc:     time.Now,
	}
	_ = os.MkdirAll(baseDir, 0o755)
	w.name = w.generateName()
	w.open()
	return w
}

func (w *rotatingWriter) open() {
	f, err := os.OpenFile(w.name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		w.logger.Error("open data log %q failed: %v", w.name, err)
		return
	}
	w.file = f
	if info, err := f.Stat(); err == nil {
		w.size = info.Size()
	}
}

// generateName builds gstoplog_GaussDB_<version>_<user>_<role>_<timestamp>.log,
// waiting up to ten seconds for the database version to be discovered.
func (w *rotatingWriter) generateName() string {
	version := "unknown"
	for i := 0; i < 10; i++ {
		version = whitespace.ReplaceAllString(w.dbInfo.Version(), "_")
		if version != "unknown" {
			break
		}
		time.Sleep(time.Second)
	}
	ts := w.nowFunc().Format("20060102_150405")
	name := "gstoplog_GaussDB_" + version + "_" + w.dbInfo.User() + "_" + w.dbInfo.Role() + "_" + ts + ".log"
	return filepath.Join(w.baseDir, name)
}

// write appends one line, rolling over first if it would exceed the size cap.
func (w *rotatingWriter) write(line string) {
	if w.file == nil {
		return
	}
	entry := line + "\n"
	if w.maxBytes > 0 && w.size+int64(len(entry)) > w.maxBytes {
		w.rollover()
	}
	if n, err := w.file.WriteString(entry); err == nil {
		w.size += int64(n)
	}
}

// rollover compresses the current file, prunes old backups, and opens a new file.
func (w *rotatingWriter) rollover() {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}
	if _, err := os.Stat(w.name); err == nil {
		w.compress(w.name)
		w.manageBackups()
	}
	w.name = w.generateName()
	w.size = 0
	w.open()
}

// compress gzips filename to filename.gz and removes the original.
func (w *rotatingWriter) compress(filename string) {
	in, err := os.Open(filename)
	if err != nil {
		w.logger.Error("Compress log_file failed: %v", err)
		return
	}
	defer in.Close()

	out, err := os.Create(filename + ".gz")
	if err != nil {
		w.logger.Error("Compress log_file failed: %v", err)
		return
	}
	gz := gzip.NewWriter(out)
	if _, err := io.Copy(gz, in); err != nil {
		w.logger.Error("Compress log_file failed: %v", err)
	}
	gz.Close()
	out.Close()
	os.Remove(filename)
}

// manageBackups keeps at most backupCount compressed backups, deleting the oldest.
func (w *rotatingWriter) manageBackups() {
	entries, err := os.ReadDir(w.baseDir)
	if err != nil {
		w.logger.Error("Manage backup log failed: %v", err)
		return
	}
	type backup struct {
		path  string
		mtime time.Time
	}
	var backups []backup
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".gz") || !strings.Contains(e.Name(), "GaussDB") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		backups = append(backups, backup{filepath.Join(w.baseDir, e.Name()), info.ModTime()})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].mtime.Before(backups[j].mtime) })
	for len(backups) > w.backupCount {
		oldest := backups[0]
		os.Remove(oldest.path)
		w.logger.Info("Remove old log file: %s", filepath.Base(oldest.path))
		backups = backups[1:]
	}
}

// close closes the current file.
func (w *rotatingWriter) close() {
	if w.file != nil {
		w.file.Close()
		w.file = nil
	}
}
