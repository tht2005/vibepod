package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"vibepod/internal/daemon"
	"vibepod/internal/proto"
)

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	foreground := fs.Bool("f", false, "log to stderr instead of the log file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	runDir := daemon.RunDir()
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)
	if !*foreground {
		f, err := os.OpenFile(filepath.Join(runDir, "daemon.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		logger = log.New(f, "", log.LstdFlags|log.Lmicroseconds)
	}
	sock := daemon.HostSock()
	if live(sock) {
		return fmt.Errorf("a daemon is already listening on %s", sock)
	}
	_ = os.Remove(sock)
	ln, err := proto.Listen(sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}
	defer os.Remove(sock)

	// Leave the running build next to the socket. A daemon outlives the binary
	// that started it, so after an upgrade the old one is still serving —
	// including spawning vpinit from its own, older image. Clients read this
	// file instead of asking, so noticing costs nothing.
	_ = os.WriteFile(filepath.Join(runDir, "version"), []byte(daemon.Version), 0o600)
	defer os.Remove(filepath.Join(runDir, "version"))

	d, err := daemon.New(runDir, logger)
	if err != nil {
		return err
	}
	stop := make(chan os.Signal, 2)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		logger.Printf("shutting down")
		ln.Close()
	}()
	logger.Printf("vibepod daemon listening on %s", sock)
	err = d.Serve(ln)
	if ne, ok := err.(*net.OpError); ok && ne.Err.Error() == "use of closed network connection" {
		return nil
	}
	return err
}

// live reports whether something is actually answering on the socket, so a
// stale file from a crashed daemon does not block startup forever.
func live(path string) bool {
	c, err := net.DialTimeout("unixpacket", path, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
