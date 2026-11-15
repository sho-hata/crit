package sandbox

import (
	"cmp"
	_ "embed"
	"fmt"
	"slices"
	"strings"
)

// baseProfile is every rule but the reads; see the file for what it allows.
//
//go:embed profile.sb
var baseProfile string

// profile renders a policy as a seatbelt profile: profile.sb, then the read
// rules, which depend on how the paths nest and so are generated. The secrets
// are denied, then each tree and the scratch are re-allowed, shallowest
// first, with the secrets beneath each denied again, so a tree under ~/.crit
// reads while a secret under a tree (a dotfiles repository at $HOME) does
// not, and the scratch under ~/.crit still reads after that. Metadata is
// readable for directories only: path walks need it, file sizes under a
// secret are nobody's business.
func profile(p policy) (text string, params []string) {
	params = []string{
		"ROOT=" + p.trees[0],
		"DEP_ROOT=" + p.trees[len(p.trees)-1],
		"SCRATCH_BASE=" + p.scratchBase,
		"SCRATCH=" + p.scratch,
	}
	param := func(key, path string) string {
		params = append(params, key+"="+path)
		return fmt.Sprintf("(subpath (param %q))", key)
	}
	var b strings.Builder
	b.WriteString(baseProfile)
	denyFilter := make(map[string]string, len(p.readDeny))
	var denies []string
	for i, path := range p.readDeny {
		denyFilter[path] = param(fmt.Sprintf("READ_DENY_%d", i), path)
		denies = append(denies, denyFilter[path])
	}
	if len(denies) > 0 {
		fmt.Fprintf(&b, "(deny file-read* %s)\n", strings.Join(denies, " "))
	}
	allows := append(slices.Clone(p.trees), p.scratch)
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
	return b.String(), params
}

// A seatbelt profile names POSIX paths whichever host renders it (the
// renderer's tests run on Windows too), so depth and under walk "/" rather
// than the host separator.
func depth(path string) int { return strings.Count(path, "/") }

func under(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}
