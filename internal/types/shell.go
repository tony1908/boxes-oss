package types

import "strings"

const shellSafeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@%+=:,./-_"

// ShellQuote quotes arg so a POSIX shell reads it back as one word.
func ShellQuote(arg string) string {
	if arg != "" && strings.IndexFunc(arg, func(r rune) bool { return !strings.ContainsRune(shellSafeChars, r) }) < 0 {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// ShellJoin joins argv into a command line that a shell parses back into the same argv.
func ShellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = ShellQuote(arg)
	}
	return strings.Join(quoted, " ")
}
