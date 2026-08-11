package review

import (
	"github.com/sho-hata/crit/internal/daemon"
	"github.com/sho-hata/crit/internal/session"
	"github.com/sho-hata/crit/internal/vcs"
)

type (
	CritJSON      = session.CritJSON
	CritJSONFile  = session.CritJSONFile
	Comment       = session.Comment
	RoundSnapshot = session.RoundSnapshot
	SessionEntry  = daemon.SessionEntry
)

var (
	ResolvedCWD = daemon.ResolvedCWD
	SessionKey  = daemon.SessionKey
	DetectVCS   = vcs.DetectVCS
)
