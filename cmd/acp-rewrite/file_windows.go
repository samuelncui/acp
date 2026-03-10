//go:build windows
// +build windows

package main

import "io/fs"

func isFileBusy(path string) (bool, error) {
	return false, nil
}

type fileIdentity struct{}

func checkFileLinked(info fs.FileInfo) (fileIdentity, bool) {
	return fileIdentity{}, false
}
