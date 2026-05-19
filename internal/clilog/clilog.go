// Package clilog gives the migrater a small, dependency-free way to print
// phase headers, status lines, and confirmation prompts. ANSI colors are
// emitted unconditionally — terminals that don't render them just show the
// raw escape sequences, which is acceptable for a one-shot operator tool.
package clilog

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	colReset  = "\x1b[0m"
	colBold   = "\x1b[1m"
	colGreen  = "\x1b[32m"
	colYellow = "\x1b[33m"
	colCyan   = "\x1b[36m"
	colRed    = "\x1b[31m"
)

// Phase prints a bold cyan section header.
func Phase(n int, total int, name string) {
	fmt.Printf("\n%s%s[%d/%d] %s%s\n", colBold, colCyan, n, total, name, colReset)
}

// Info prints a regular status line indented under the current phase.
func Info(format string, a ...any) {
	fmt.Printf("  %s\n", fmt.Sprintf(format, a...))
}

// OK prints a green ✓ line.
func OK(format string, a ...any) {
	fmt.Printf("  %s✓%s %s\n", colGreen, colReset, fmt.Sprintf(format, a...))
}

// Warn prints a yellow ! line.
func Warn(format string, a ...any) {
	fmt.Printf("  %s!%s %s\n", colYellow, colReset, fmt.Sprintf(format, a...))
}

// Fail prints a red ✗ line.
func Fail(format string, a ...any) {
	fmt.Printf("  %s✗%s %s\n", colRed, colReset, fmt.Sprintf(format, a...))
}

// Confirm asks the user a yes/no question. assumeYes shortcuts to true.
// EOF (piped input) is treated as "no" for safety. Reads from stdin.
func Confirm(prompt string, assumeYes bool) bool {
	if assumeYes {
		return true
	}
	fmt.Printf("\n%s%s [y/N]: %s", colBold, prompt, colReset)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// Section writes a horizontal rule to the given writer — used between
// sub-blocks of teardown output where Phase would be too loud.
func Section(w io.Writer, label string) {
	fmt.Fprintf(w, "\n--- %s ---\n", label)
}
