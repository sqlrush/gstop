// Package persist implements gstop's monitoring-data persistence: a background
// logger that writes the OS, DB, and instance panel values as fixed-width,
// pipe-separated rows to size-rotated, gzip-compressed files. Port of
// common/data_logger.py.
package persist

import (
	"strings"
	"sync"
	"time"

	"gstop/internal/config"
	"gstop/internal/logging"
	"gstop/internal/model"
)

// headerEvery is how many rows are written between repeated column headers.
const headerEvery = 30

// LogSource is a panel that can contribute a row of loggable columns.
type LogSource interface {
	LogFields() (items, values []string, widths []int)
}

// DataLogger periodically snapshots the OS, DB, and instance panels and appends a
// formatted row to a rotating log file. Port of data_logger.DataLogger.
type DataLogger struct {
	osSrc  LogSource
	dbSrc  LogSource
	insSrc LogSource

	cfg      *config.Config
	logger   *logging.Logger
	interval time.Duration

	counter int
	title   []string
	widths  []int
	writer  *rotatingWriter

	done chan struct{}
	wg   sync.WaitGroup
}

// New builds a DataLogger. dbInfo names the output files with the connected
// instance's version/user/role.
func New(osSrc, dbSrc, insSrc LogSource, dbInfo *model.DBInfo, cfg *config.Config, logger *logging.Logger, interval time.Duration) *DataLogger {
	baseDir := cfg.GetString("main.persist_file_base_dir", "logs")
	maxBytes := int64(cfg.GetInt("main.persist_file_max_size", 5)) * 1024 * 1024
	backups := cfg.GetInt("main.max_backup_count", 30)
	return &DataLogger{
		osSrc:    osSrc,
		dbSrc:    dbSrc,
		insSrc:   insSrc,
		cfg:      cfg,
		logger:   logger,
		interval: interval,
		writer:   newRotatingWriter(dbInfo, baseDir, maxBytes, backups, logger),
		done:     make(chan struct{}),
	}
}

// Start launches the background logging goroutine.
func (d *DataLogger) Start() {
	d.wg.Add(1)
	go d.run()
}

// run writes one row per interval until Stop closes done.
func (d *DataLogger) run() {
	defer d.wg.Done()
	for {
		d.writeRow()
		timer := time.NewTimer(d.interval)
		select {
		case <-d.done:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// writeRow assembles and appends one data row, emitting the header every
// headerEvery rows. Order is OS, DB, then instance columns, matching the original.
func (d *DataLogger) writeRow() {
	osItems, osValues, osWidths := d.osSrc.LogFields()
	dbItems, dbValues, dbWidths := d.dbSrc.LogFields()
	insItems, insValues, insWidths := d.insSrc.LogFields()

	values := concat(osValues, dbValues, insValues)
	if d.title == nil {
		d.title = concat(osItems, dbItems, insItems)
		d.widths = concatInts(osWidths, dbWidths, insWidths)
		d.title = padAll(d.title, d.widths)
	}

	if d.counter%headerEvery == 0 {
		d.counter = 0
		d.writeLine(strings.Join(d.title, "|"))
	}
	d.writeLine(strings.Join(padAll(values, d.widths), "|"))
	d.counter++
}

// writeLine prepends the timestamp and writes one record.
func (d *DataLogger) writeLine(body string) {
	line := time.Now().Format("2006-01-02 15:04:05") + " - " + body
	d.writer.write(line)
}

// Stop ends the logging goroutine and closes the file.
func (d *DataLogger) Stop() {
	d.logger.Warning("The data logger thread is starting to exit.")
	close(d.done)
	d.wg.Wait()
	d.writer.close()
	d.logger.Warning("The data logger thread has exited.")
}

// padAll left-justifies each field to its column width.
func padAll(fields []string, widths []int) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		w := 0
		if i < len(widths) {
			w = widths[i]
		}
		out[i] = leftJustify(f, w)
	}
	return out
}

func leftJustify(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func concat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func concatInts(parts ...[]int) []int {
	var out []int
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
