package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// One binary, two roles. argv[0] basename selects:
//
//   git-remote-ogorod <remote> <url>   ← git invokes this when it sees
//                                        ogorod://<repo> remotes
//   ogorod <subcommand> [args...]       ← admin CLI (gc, ...)
//
// Symlink the same binary to both names on install.

func main() {
	exc := Try(func() {
		switch filepath.Base(os.Args[0]) {
		case "git-remote-ogorod":
			helperMain(os.Args[1:])
		default:
			adminMain(os.Args[1:])
		}
	})

	exc.Catch(func(e *Exception) {
		fmt.Fprintln(os.Stderr, "ogorod:", e.Error())
		os.Exit(1)
	})
}

func helperMain(args []string) {
	ThrowFmt("git-remote-ogorod: not implemented yet")
}

func adminMain(args []string) {
	if len(args) < 1 {
		ThrowFmt("usage: ogorod {gc} [args...]")
	}

	switch args[0] {
	case "gc":
		ThrowFmt("ogorod gc: not implemented yet")
	default:
		ThrowFmt("unknown subcommand: %q", args[0])
	}
}
