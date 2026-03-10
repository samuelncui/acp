//go:build darwin || linux
// +build darwin linux

package main

import (
	"io/fs"
	"os"
	"syscall"
)

func isFileBusy(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return true, nil
		}
		return false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return false, err
	}
	return false, nil
}

type fileIdentity struct {
	dev   uint64
	ino   uint64
	nlink uint64
}

func checkFileLinked(info fs.FileInfo) (fileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return fileIdentity{}, false
	}
	if stat.Nlink <= 1 {
		return fileIdentity{}, false
	}
	return fileIdentity{
		dev:   uint64(stat.Dev),
		ino:   stat.Ino,
		nlink: uint64(stat.Nlink),
	}, true
}
