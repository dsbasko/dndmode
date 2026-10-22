//go:build linux

package main

import "golang.org/x/sys/unix"

// dupOnto makes newfd a copy of oldfd. dup3 rather than dup2: the latter
// does not exist on every Linux architecture (arm64 has only dup3).
func dupOnto(oldfd, newfd int) error { return unix.Dup3(oldfd, newfd, 0) }
