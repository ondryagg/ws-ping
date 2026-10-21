//go:build !linux

package sysinfo

import "errors"

var errUnsupported = errors.New("sysinfo: unsupported platform")

func OpenFDs() (int, error)    { return 0, errUnsupported }
func FDLimit() (uint64, error) { return 0, errUnsupported }
