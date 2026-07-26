package gsbench

import "fmt"

const Author = "WangYingJie <sqlrush@gmail.com>"

// Banner is the mandatory preamble for terminal output, version output, and
// every newly-created background log.
func Banner(version string) string {
	return fmt.Sprintf("gsbench %s\nAuthor: %s\n", version, Author)
}
