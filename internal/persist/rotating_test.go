package persist

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gstop/internal/model"
)

func TestGenerateNameUsesOneDBInfoSnapshot(t *testing.T) {
	info := model.NewDBInfo()
	a := model.DBInfoSnapshot{Version: "version-a", User: "user-a", Role: "role-a"}
	b := model.DBInfoSnapshot{Version: "version-b", User: "user-b", Role: "role-b"}
	info.SetSnapshot(a)
	now := time.Date(2026, 7, 24, 12, 34, 56, 0, time.Local)
	w := &rotatingWriter{
		dbInfo: info, baseDir: t.TempDir(),
		nowFunc: func() time.Time { return now },
	}
	valid := map[string]bool{
		"gstoplog_GaussDB_version-a_user-a_role-a_20260724_123456.log": true,
		"gstoplog_GaussDB_version-b_user-b_role-b_20260724_123456.log": true,
	}

	const iterations = 100_000
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				info.SetSnapshot(b)
			} else {
				info.SetSnapshot(a)
			}
		}
	}()

	for i := 0; i < iterations; i++ {
		name := filepath.Base(w.generateName())
		if !valid[name] {
			t.Fatalf("mixed DBInfo filename at iteration %d: %q", i, name)
		}
	}
	wg.Wait()
}
