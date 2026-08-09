package preview

import (
	"github.com/sho-hata/crit/internal/daemon"
	"github.com/sho-hata/crit/internal/review"
	"github.com/sho-hata/crit/internal/server"
	"github.com/sho-hata/crit/internal/session"
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
