package main

import (
	"os"

	integrationassets "github.com/sho-hata/crit/integrations"
	"github.com/sho-hata/crit/internal/clicmd"
	"github.com/sho-hata/crit/internal/session"
	webassets "github.com/sho-hata/crit/web"
)

var frontendFS = webassets.FS
var integrationsFS = integrationassets.FS

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if len(os.Args) < 2 {
		clicmd.Exit(session.RunReview(nil))
		return
	}
	if handled, err := dispatchCLI(os.Args[1:]); handled {
		clicmd.Exit(err)
		return
	}
	args := resolveAtPrefixedArgs(os.Args[1:])
	runPositionalCLI(args)
}
