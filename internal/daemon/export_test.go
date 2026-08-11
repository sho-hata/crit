package daemon

import "github.com/sho-hata/crit/internal/config"

type commonDaemonFlags = CommonDaemonFlags

var (
	atomicWriteFile         = config.AtomicWriteFile
	appendCommonDaemonFlags = AppendCommonDaemonFlags
)
