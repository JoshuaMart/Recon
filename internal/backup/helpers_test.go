//go:build integration

package backup_test

import (
	"io"
	"strconv"
	"strings"
	"unicode"
)

// readAll drains a container exec stream. The multiplexed stream carries an
// 8-byte header per frame, and the counts read here are short enough that
// stripping the non-printable bytes is both simpler and sufficient.
func readAll(r io.Reader) string {
	if r == nil {
		return ""
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range string(raw) {
		if unicode.IsPrint(c) || c == '\n' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func parseCount(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return -1
	}
	return n
}
