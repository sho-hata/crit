package lsp

import (
	"os"
	"strconv"
	"strings"
)

// processDead reports a pid that is gone or a zombie (state Z or X).
func processDead(pid int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	// The command name is parenthesised and may hold spaces; the state
	// follows the closing parenthesis.
	rest := raw[strings.LastIndexByte(string(raw), ')')+1:]
	fields := strings.Fields(string(rest))
	return len(fields) > 0 && (fields[0] == "Z" || fields[0] == "X")
}
