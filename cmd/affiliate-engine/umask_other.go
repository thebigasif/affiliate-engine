//go:build !unix

package main

// setUmask is a no-op off unix (Windows has no umask; ACLs govern access).
func setUmask() {}
