package main

import (
	"github.com/sho-hata/crit/internal/clicmd"
	"github.com/sho-hata/crit/internal/comment"
	"github.com/sho-hata/crit/internal/config"
	"github.com/sho-hata/crit/internal/github"
	"github.com/sho-hata/crit/internal/live"
	"github.com/sho-hata/crit/internal/preview"
	"github.com/sho-hata/crit/internal/review"
	"github.com/sho-hata/crit/internal/session"
)

func runConfig(args []string)   { clicmd.Exit(config.RunConfig(args)) }
func runPull(args []string)     { clicmd.Exit(github.RunPull(args)) }
func runPush(args []string)     { clicmd.Exit(github.RunPush(args)) }
func runComment(args []string)  { clicmd.Exit(comment.RunComment(args)) }
func runComments(args []string) { clicmd.Exit(comment.RunComments(args)) }
func runReview(args []string)   { clicmd.Exit(session.RunReview(args)) }
func runPlan(args []string)     { clicmd.Exit(session.RunPlan(args)) }
func runStop(args []string)     { clicmd.Exit(session.RunStop(args)) }
func runStatus(args []string)   { clicmd.Exit(session.RunStatus(args)) }
func runCleanup(args []string)  { clicmd.Exit(review.RunCleanup(args)) }
func runPR(args []string)       { clicmd.Exit(github.RunPR(args)) }

func runLive(args []string)    { live.RunLive(args) }
func runPreview(args []string) { preview.RunPreview(args) }
func runStats(args []string)   { session.RunStats(args) }

func runPlanHook()      { clicmd.Exit(session.RunPlanHook()) }
func runCodexPlanHook() { clicmd.Exit(session.RunCodexPlanHook()) }
