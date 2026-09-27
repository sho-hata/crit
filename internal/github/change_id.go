package github

// ChangeID identifies a GitHub pull request. Number is the repository-local
// PR number. Project ("owner/repo") and Host are populated when a --pr URL
// points at a repository other than the current checkout, so lookups can be
// pinned with -R instead of resolving against the cwd remote.
type ChangeID struct {
	Number  int
	Project string
	Host    string
}
