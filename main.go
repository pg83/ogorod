package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// One binary, two entry points dispatched by argv[0] basename:
//
//   git-remote-ogorod <remote> <url>   invoked by git when it sees
//                                        ogorod://<repo> remotes
//   ogorod <subcommand> [args...]      admin CLI (gc, …)
//
// Install both names as symlinks (or hardlinks) to the same file.
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

func adminMain(args []string) {
	if len(args) < 1 {
		ThrowFmt("usage: ogorod {serve|hook|gc|repack} [args...]")
	}

	switch args[0] {
	case "serve":
		serveMain(args[1:])
	case "hook":
		hookMain(args[1:])
	case "gc":
		gcMain(args[1:])
	case "repack":
		repackMain(args[1:])
	default:
		ThrowFmt("unknown subcommand: %q", args[0])
	}
}
