//go:build !windows

package platform

import "os"

func isPrivileged() bool { return os.Geteuid() == 0 }
