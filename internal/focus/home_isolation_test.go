package focus

import (
	"os"
	"testing"

	"github.com/sho-hata/crit/internal/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.RunWithTempHome(m)) }
