package main

import (
	"github.com/sho-hata/crit/internal/vcs"
)

func resetDefaultBranchOnce() {
	vcs.ResetDefaultBranchOnceForTest()
}
