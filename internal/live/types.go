package live

import (
	"github.com/sho-hata/crit/internal/comment"
	"github.com/sho-hata/crit/internal/config"
	"github.com/sho-hata/crit/internal/github"
	"github.com/sho-hata/crit/internal/review"
	"github.com/sho-hata/crit/internal/server"
	"github.com/sho-hata/crit/internal/session"
	"github.com/sho-hata/crit/internal/testutil"
)

var writeFile = testutil.WriteFile

type (
	Config       = config.Config
	CritJSON     = session.CritJSON
	CritJSONFile = session.CritJSONFile
	Session      = session.Session
	FileEntry    = session.FileEntry
	Comment      = session.Comment
	DOMAnchor    = session.DOMAnchor
	SSEEvent     = session.SSEEvent
)

var (
	looksLikeLiveArgs      = LooksLikeLiveArgs
	saveCritJSON           = review.SaveCritJSON
	loadCritJSON           = review.LoadCritJSON
	appendReply            = comment.AppendReply
	checkCommentCLIAllowed = comment.CheckCommentCLIAllowed
	carryForwardComment    = session.CarryForwardComment
	NewServer              = server.NewServer
	frontendFS             = server.FrontendFS
)

type GhComment = github.GhComment

var mergeGHComments = github.MergeGHComments
