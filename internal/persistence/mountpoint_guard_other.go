//go:build !linux

package persistence

func protectMountDir(string) error   { return nil }
func unprotectMountDir(string) error { return nil }
