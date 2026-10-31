//go:build unix

package main

import "syscall"

// setUmask makes every file this process creates owner-only (the SQLite
// database and its -wal/-shm siblings, backups, the generated secret):
// the money database must never be group/world readable by default.
func setUmask() { syscall.Umask(0o077) }
