// vpsh is the shim. It is bind-mounted over a binary the pod should not run
// itself, so the kernel reaches it instead of the real program.
//
// It is a separate binary rather than another name for vibepod because a shim
// is never invoked under its own name: argv[0] is whatever the caller passed.
// /proc/self/exe, which resolves through the bind mount, is what tells it
// which program it is standing in for.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"unsafe"

	"vibepod/internal/pod"
	"vibepod/internal/proto"
)

// exitUnavailable is EX_TEMPFAIL: vibepod could not place this command.
// Distinct from anything the command itself would return.
const exitUnavailable = 75

func main() {
	self, err := os.Readlink("/proc/self/exe")
	if err != nil {
		fail("cannot identify the shimmed binary: %v", err)
	}
	// If vibepod was upgraded on disk while this pod was running, the kernel
	// marks the shim's inode as unlinked and appends this suffix. The path is
	// still the one the daemon shimmed.
	self = strings.TrimSuffix(self, " (deleted)")
	sock := os.Getenv("VIBEPOD_SOCK")
	if sock == "" {
		sock = pod.SockPath
	}
	c, err := proto.Dial(sock)
	if err != nil {
		fail("cannot reach the daemon at %s: %v", sock, err)
	}
	defer c.Close()

	cwd, _ := os.Getwd()
	m := &proto.Msg{
		Op:      proto.OpExec,
		Path:    self,
		Argv:    os.Args,
		Cwd:     cwd,
		Env:     os.Environ(),
		Session: os.Getenv("VIBEPOD_SESSION"),
		TTY:     isatty(0),
	}
	// Hand over our own stdio. Whatever runs this command, here or on another
	// machine, writes straight to the caller's terminal or pipe.
	if err := c.Send(m, 0, 1, 2); err != nil {
		fail("cannot send the command: %v", err)
	}
	// A routed command runs on another machine, where ssh will not forward a
	// Ctrl-C without a PTY. Pass signals to the daemon instead; it kills the
	// remote process over a second multiplexed channel.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			_ = c.Send(&proto.Msg{Op: proto.OpSignal, Sig: int(s.(syscall.Signal))})
		}
	}()

	reply, _, err := c.Recv()
	if err != nil {
		fail("no answer from the daemon: %v", err)
	}
	signal.Stop(sigs)
	switch reply.Op {
	case proto.OpRunLocal:
		// Run the original, which the daemon stashed before shadowing it.
		if err := syscall.Exec(reply.Path, os.Args, os.Environ()); err != nil {
			fail("cannot run %s: %v", reply.Path, err)
		}
	case proto.OpExit:
		os.Exit(reply.Code)
	case proto.OpErr:
		fail("%s", reply.Err)
	}
	fail("unexpected reply %q", reply.Op)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "vibepod: "+format+"\n", a...)
	os.Exit(exitUnavailable)
}

func isatty(fd uintptr) bool {
	var t [64]byte
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS,
		uintptr(unsafe.Pointer(&t[0])))
	return errno == 0
}
