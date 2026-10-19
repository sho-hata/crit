package server

import (
	"os"
	"testing"

	"github.com/sho-hata/crit/internal/testutil"
)

func TestMain(m *testing.M) {
	cleanup := testutil.PrependFailingForgeCLIs()
	code := testutil.RunWithTempHome(m)
	cleanup()
	os.Exit(code)
}
