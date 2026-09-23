// Command vibepod is the whole program. It answers to several names:
//
//	vpctl    the user-facing client
//	vibepod  the user daemon (vibepod daemon)
//	vpinit   PID 1 inside a pod (vibepod vpinit)
//
// vpsh is a separate binary; see its package comment for why.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"vibepod/internal/pod"
)

func main() {
	args := os.Args[1:]
	switch filepath.Base(os.Args[0]) {
	case "vpctl":
		os.Exit(runCtl(args))
	}
	if len(args) > 0 {
		switch args[0] {
		case "vpinit":
			if err := pod.RunInit(); err != nil {
				fmt.Fprintln(os.Stderr, "vpinit:", err)
				os.Exit(1)
			}
			return
		case "daemon":
			if err := runDaemon(args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "vibepod:", err)
				os.Exit(1)
			}
			return
		}
	}
	os.Exit(runCtl(args))
}
