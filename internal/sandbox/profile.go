package sandbox

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// profile renders a policy as a seatbelt profile. Paths travel as -D
// parameters, never spliced into the text. Rules are last-match-wins: the
// secrets are denied, then each readAllow entry is re-allowed, shallowest
// first, and the secrets beneath it denied again, so a tree under ~/.crit
// reads while a secret under a tree (a dotfiles repository at $HOME) does
// not, and the scratch under ~/.crit still reads after that. Execution and
// native-code mapping are denied under the trees and the scratch. Metadata
// is readable for directories only: path walks need it, file sizes under a
// secret are nobody's business. The mach-lookup list is what the measured
// servers (gopls, typescript-language-server, pyright, intelephense,
// terraform-ls) need to answer a hover; everything else is denied.
func profile(p policy) (text string, params []string) {
	param := func(key, path string) string {
		params = append(params, key+"="+path)
		return fmt.Sprintf("(subpath (param %q))", key)
	}
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n(allow process-fork)\n(allow process-exec)\n")
	scratch := param("SCRATCH_0", p.scratch)
	noExec := []string{}
	for i, path := range p.execDeny {
		noExec = append(noExec, param(fmt.Sprintf("EXEC_DENY_%d", i), path))
	}
	noExec = append(noExec, scratch)
	fmt.Fprintf(&b, "(deny process-exec %s)\n(allow file-map-executable)\n(deny file-map-executable %s)\n",
		strings.Join(noExec, " "), strings.Join(noExec, " "))
	b.WriteString("(allow signal (target same-sandbox))\n(allow file-read*)\n")
	denyFilter := make(map[string]string, len(p.readDeny))
	var denies []string
	for i, path := range p.readDeny {
		denyFilter[path] = param(fmt.Sprintf("READ_DENY_%d", i), path)
		denies = append(denies, denyFilter[path])
	}
	if len(denies) > 0 {
		fmt.Fprintf(&b, "(deny file-read* %s)\n", strings.Join(denies, " "))
	}
	allows := slices.Clone(p.readAllow)
	slices.SortStableFunc(allows, func(a, b string) int { return cmp.Compare(depth(a), depth(b)) })
	for i, root := range allows {
		fmt.Fprintf(&b, "(allow file-read* %s)\n", param(fmt.Sprintf("READ_ALLOW_%d", i), root))
		var nested []string
		for _, d := range p.readDeny {
			if under(d, root) {
				nested = append(nested, denyFilter[d])
			}
		}
		if len(nested) > 0 {
			fmt.Fprintf(&b, "(deny file-read* %s)\n", strings.Join(nested, " "))
		}
	}
	b.WriteString("(allow file-read-metadata (vnode-type DIRECTORY))\n")
	fmt.Fprintf(&b, "(allow file-write* %s)\n", scratch)
	b.WriteString(`(allow file-write-data (literal "/dev/null") (literal "/dev/zero"))
(allow file-ioctl (literal "/dev/null") (literal "/dev/zero") (literal "/dev/random") (literal "/dev/urandom") (literal "/dev/dtracehelper"))
(allow sysctl-read)
(allow ipc-posix-shm)
(allow ipc-posix-sem)
(allow user-preference-read)
(allow iokit-get-properties)
(allow mach-lookup
  (global-name "com.apple.logd")
  (global-name "com.apple.system.logger")
  (global-name "com.apple.system.notification_center")
  (global-name "com.apple.system.opendirectoryd.libinfo")
  (global-name "com.apple.system.opendirectoryd.membership")
  (global-name "com.apple.FSEvents"))
`)
	return b.String(), params
}

// A seatbelt profile names POSIX paths whichever host renders it (the
// renderer's tests run on Windows too), so depth and under walk "/" rather
// than the host separator.
func depth(path string) int { return strings.Count(path, "/") }

func under(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}
