// Command vibepod is the whole program. It answers to four names:
//
//	vibepod  the cockpit and the pod's lifecycle — and the user daemon
//	vp       the verb surface, inside a pod and out
//	vpinit   PID 1 inside a pod (vibepod vpinit)
//
// vpsh is a separate binary: it is the pod's $SHELL, and a shell that linked in
// the whole client would be a strange thing to put on the front of every command.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"vibepod/internal/daemon"
	"vibepod/internal/node"
	"vibepod/internal/pod"
)

func main() {
	args := os.Args[1:]
	name := filepath.Base(os.Args[0])
	if len(args) > 0 {
		switch args[0] {
		case "vpinit":
			if err := pod.RunInit(); err != nil {
				fmt.Fprintln(os.Stderr, "vpinit:", err)
				os.Exit(1)
			}
			return
		// The three names a node answers to. They are reached over ssh by the
		// daemon on your machine, never typed.
		case "vpnode":
			if err := node.Run(args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "vpnode:", err)
				os.Exit(1)
			}
			return
		case "nodeexec":
			if err := node.Exec(args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "nodeexec:", err)
				os.Exit(exitUnavailable)
			}
			return
		case "sftp-local":
			if err := node.SFTPLocal(); err != nil {
				fmt.Fprintln(os.Stderr, "sftp-local:", err)
				os.Exit(1)
			}
			return
		case "nodectl":
			if err := node.Ctl(args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "nodectl:", err)
				os.Exit(1)
			}
			return
		case "nodedown":
			if err := node.Down(args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "nodedown:", err)
				os.Exit(1)
			}
			return
		case "version":
			fmt.Println(daemon.Version)
			return
		case "daemon":
			if err := runDaemon(args[1:]); err != nil {
				fmt.Fprintln(os.Stderr, "vibepod:", err)
				os.Exit(1)
			}
			return
		}
	}
	if name == "vpctl" {
		fmt.Fprintln(os.Stderr, "vibepod: vpctl is now `vp` (and `vibepod` for the "+
			"cockpit, up, down and doctor)")
	}
	os.Exit(runCli(args, name != "vp"))
}
