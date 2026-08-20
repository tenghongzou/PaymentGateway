package app

import (
	"os"
	"syscall"
)

func findSelf() (*os.Process, error) { return os.FindProcess(os.Getpid()) }

func sigterm() os.Signal { return syscall.SIGTERM }
