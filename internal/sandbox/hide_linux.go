package sandbox

import "golang.org/x/sys/unix"

// HideFromSameUser makes this process non-dumpable (PR_SET_DUMPABLE 0), so
// /proc/<pid>/environ, maps and the like answer only to root. The daemon's
// environment carries whatever the reviewer's shell exported (live-mode
// cookies, tokens) and the confined servers run under the same uid; the
// path-based sandbox cannot close procfs for them, this does. The cost: no
// core dumps and no ptrace of the daemon without root. Children exec with
// the flag reset, so the servers themselves are unaffected.
func HideFromSameUser() {
	_ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
