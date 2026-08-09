package comment

import (
	"github.com/sho-hata/crit/internal/review"
	"github.com/sho-hata/crit/internal/session"
)

type (
	CritJSON       = session.CritJSON
	CritJSONFile   = session.CritJSONFile
	Comment        = session.Comment
	DOMAnchor      = session.DOMAnchor
	Reply          = session.Reply
	Focus          = session.Focus
	inheritedScope = session.InheritedScope
)

const (
	FocusRange         = session.FocusRange
	FocusWorkingTree   = session.FocusWorkingTree
	DiffScopeLayer     = session.DiffScopeLayer
	DiffScopeFullStack = session.DiffScopeFullStack
)

var (
	reviewPathsFor = session.ReviewPathsFor
	loadCritJSON   = review.LoadCritJSON
	saveCritJSON   = review.SaveCritJSON
)
