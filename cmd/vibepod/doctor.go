package main

import (
	"fmt"
	"os"
	"strings"
)

func checkUserns() error {
	b, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
	if err != nil {
		return nil // the knob is absent on some kernels; assume available
	}
	if strings.TrimSpace(string(b)) == "0" {
		return fmt.Errorf("disabled (user.max_user_namespaces = 0)")
	}
	return nil
}

func checkSeccomp() error {
	b, err := os.ReadFile("/proc/sys/kernel/seccomp/actions_avail")
	if err != nil {
		return fmt.Errorf("cannot read seccomp actions: %w", err)
	}
	if !strings.Contains(string(b), "user_notif") {
		return fmt.Errorf("this kernel has no SECCOMP_RET_USER_NOTIF")
	}
	return nil
}
