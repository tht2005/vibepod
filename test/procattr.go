package e2e

import "syscall"

// A session leader of its own, so signals sent to the test process group do
// not reach the client under test.
var syscallSysProcAttr = syscall.SysProcAttr{Setsid: true}
