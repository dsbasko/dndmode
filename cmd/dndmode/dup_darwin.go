//go:build darwin

package main

import "golang.org/x/sys/unix"

// dupOnto makes newfd a copy of oldfd (dup2).
func dupOnto(oldfd, newfd int) error { return unix.Dup2(oldfd, newfd) }
