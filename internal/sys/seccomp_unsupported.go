//go:build linux && !amd64

package sys

import "errors"

const NotifRespContinue = 1

type Notif struct {
	ID   uint64
	PID  uint32
	NR   int32
	Args [6]uint64
}

var errArch = errors.New("vibepod: the exec gate is implemented for amd64 only")

func InstallExecGate() (int, error)                                   { return -1, errArch }
func NotifRecv(int) (*Notif, error)                                   { return nil, errArch }
func NotifSend(int, uint64, int64, int32, uint32) error               { return errArch }
func NotifContinue(int, uint64) error                                 { return errArch }
func NotifIDValid(int, uint64) bool                                   { return false }
