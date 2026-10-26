//go:build !darwin || !cgo

package main

func readKeychainToken() (string, error) { return "", nil }
