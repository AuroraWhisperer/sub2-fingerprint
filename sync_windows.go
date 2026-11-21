//go:build windows

package main

// The production target is Linux; Windows does not support directory fsync.
func syncDir(string) error { return nil }
