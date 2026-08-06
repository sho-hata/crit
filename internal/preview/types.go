package preview

import (
	"github.com/tomasz-tomczyk/crit/internal/daemon"
	"github.com/tomasz-tomczyk/crit/internal/review"
	"github.com/tomasz-tomczyk/crit/internal/server"
	"github.com/tomasz-tomczyk/crit/internal/session"
)

type (
	Server       = server.Server
	Session      = server.Session
	CritJSON     = session.CritJSON
	CritJSONFile = session.CritJSONFile
	Comment      = session.Comment
	DOMAnchor    = session.DOMAnchor
	FileEntry    = session.FileEntry
	SSEEvent     = session.SSEEvent
)

var (
	saveCritJSON         = review.SaveCritJSON
	frontendFS           = server.FrontendFS
	liveSessionKey       = daemon.LiveSessionKey
	NewServer            = server.NewServer
	previewSessionKey    = PreviewSessionKey
	looksLikePreviewArgs = LooksLikePreviewArgs
)

type serverConfig struct {
	previewFile string
	reviewPath  string
}

func createPreviewSession(sc *serverConfig) (*Session, error) {
	return session.NewPreviewSession(sc.previewFile, sc.reviewPath)
}
