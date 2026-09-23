package proto

import "os"

func osNewFile(fd int, name string) *os.File { return os.NewFile(uintptr(fd), name) }
