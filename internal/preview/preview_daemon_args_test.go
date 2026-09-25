package preview

import (
	"reflect"
	"testing"

	"github.com/sho-hata/crit/internal/config"
)

func TestBuildPreviewStartArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		port           int
		host           string
		publicURL      string
		allowUnauthNet bool
		noOpen         bool
		quiet          bool
		cfg            config.Config
		want           []string
	}{
		{
			name:      "CLI public URL overrides config",
			publicURL: "https://cli.ts.net",
			noOpen:    true,
			cfg:       config.Config{PublicURL: "https://config.ts.net"},
			want: []string{
				"--preview-file", "/tmp/page.html",
				"--public-url", "https://cli.ts.net",
				"--no-open",
			},
		},
		{
			name: "config public URL",
			cfg:  config.Config{PublicURL: "https://config.ts.net"},
			want: []string{
				"--preview-file", "/tmp/page.html",
				"--public-url", "https://config.ts.net",
			},
		},
		{
			name: "all explicit flags",
			port: 4242, host: "0.0.0.0", publicURL: "https://preview.example/base/",
			allowUnauthNet: true, noOpen: true, quiet: true,
			want: []string{
				"--preview-file", "/tmp/page.html",
				"--port", "4242",
				"--host", "0.0.0.0",
				"--public-url", "https://preview.example/base/",
				"--allow-unauthenticated-network",
				"--no-open",
				"--quiet",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildPreviewStartArgsForTest(
				"/tmp/page.html", tt.port, tt.host, tt.publicURL, tt.allowUnauthNet,
				tt.noOpen, tt.quiet, tt.cfg,
			)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildPreviewStartArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}
