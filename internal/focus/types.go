package focus

import "github.com/sho-hata/crit/internal/session"

type (
	Focus          = session.Focus
	FocusKind      = session.FocusKind
	DiffScope      = session.DiffScope
	CritJSON       = session.CritJSON
	InheritedScope = session.InheritedScope
)

const (
	FocusWorkingTree   = session.FocusWorkingTree
	FocusRange         = session.FocusRange
	DiffScopeLayer     = session.DiffScopeLayer
	DiffScopeFullStack = session.DiffScopeFullStack
)
