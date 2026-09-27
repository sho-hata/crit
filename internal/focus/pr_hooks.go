package focus

import "github.com/sho-hata/crit/internal/vcs"

// PRResolveInfo carries PR metadata needed to build a Focus without importing github.
type PRResolveInfo struct {
	URL               string
	Number            int
	Title             string
	BaseRefOid        string
	HeadRefOid        string
	BaseRefName       string
	HeadRefName       string
	HeadRepoURL       string
	IsCrossRepository bool
}

var (
	// FetchPRHook resolves a --pr <num|url> spec. The raw CLI value is passed
	// through so URL-derived owner/repo survives.
	FetchPRHook     func(spec string) (PRResolveInfo, error)
	IsStackedPRHook func(info PRResolveInfo, v vcs.VCS) bool
)

// SetPRResolveHooks wires PR resolution from cmd/crit to break focus↔github cycles.
func SetPRResolveHooks(
	fetch func(spec string) (PRResolveInfo, error),
	stacked func(info PRResolveInfo, v vcs.VCS) bool,
) {
	FetchPRHook = fetch
	IsStackedPRHook = stacked
}
