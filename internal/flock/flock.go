// Package flock takes exclusive OS file locks: flock(2) on Unix, LockFileEx
// on Windows. The kernel drops a lock when its holder exits, so a crashed
// process never leaves one behind.
package flock
