package gsbench

import (
	"strings"
	"testing"
)

func TestBannerStartsWithVersionAndAuthor(t *testing.T) {
	got := Banner("v0.1.0")
	want := "gsbench v0.1.0\nAuthor: WangYingJie <sqlrush@gmail.com>\n"
	if got != want {
		t.Fatalf("banner = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "gsbench ") {
		t.Fatalf("banner prefix = %q", got)
	}
}
