// PR-B.2 matter-driven task execution. Daemon pulls tasks from matter
// (instead of receiving them via fleet heartbeat) and writes back the
// reply, activity, and ack all directly to matter.
package internal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-daemon-cli/internal/adapter"
)

// handleMatterBotTask runs the agent for a matter-pulled task and posts
// the reply + activity + ack back to matter directly.
//nolint:unused
func (d *Daemon) handleMatterBotTask(parent context.Context, workspaceID string, task MatterBotTask) {
	log.Printf("[INFO] [matter-task] received id=%s workspace=%s matter=%s bot=%s",
		task.ID, workspaceID, task.MatterID, task.BotUID)

	if workspaceID == "" {
		d.ackMatterTask(parent, task, "failed", "", "missing workspace_id")
		return
	}
	if strings.TrimSpace(task.Prompt) == "" {
		d.ackMatterTask(parent, task, "failed", "", "empty prompt")
		return
	}

	start := time.Now()
	ad, err := d.runtimeAdapter("")
	if err != nil {
		d.ackMatterTask(parent, task, "failed", "", fmt.Sprintf("resolve adapter: %v", err))
		return
	}
	res, err := ad.RunTask(parent, adapter.RunTaskRequest{
		WorkspaceID: workspaceID,
		BotUID:      task.BotUID,
		Prompt:      task.Prompt,
		TaskID:      task.ID,
		MatterID:    task.MatterID,
	})
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		log.Printf("[ERROR] [matter-task] id=%s openclaw agent failed: %v", task.ID, err)
		d.matterWriteFailedActivity(parent, task, err.Error(), elapsed)
		d.ackMatterTask(parent, task, "failed", "", err.Error())
		return
	}
	reply := truncateOutput(res.Reply, maxResultSummaryBytes)
	if err := d.matterWriteReplyAndActivity(parent, task, reply, elapsed); err != nil {
		log.Printf("[WARN] [matter-task] id=%s writeback failed: %v (ack still attempted)", task.ID, err)
	}
	log.Printf("[INFO] [matter-task] id=%s succeeded, %d bytes reply, %dms", task.ID, len(reply), elapsed)
	d.ackMatterTask(parent, task, "succeeded", reply, "")
}

//nolint:unused
func (d *Daemon) matterWriteReplyAndActivity(parent context.Context, task MatterBotTask, reply string, elapsedMs int64) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	// 合并 plan 决策一+二 Phase 4 + polish: matter 端 AU5 已删, daemon
	// 不再需要传 task_id / claim_token 给 timeline/activity writeback.
	// (ack endpoint 仍需 claim_token — 那是 atomic claim 标识, 不是 AU5.)
	if err := d.client.WriteMatterTimeline(ctx, task.MatterID, task.BotUID, task.SpaceID, reply); err != nil {
		return err
	}
	return d.client.WriteMatterActivity(ctx, task.MatterID, task.BotUID, "agent_task_completed",
		map[string]any{
			"bot_uid":    task.BotUID,
			"task_id":    task.ID,
			"elapsed_ms": elapsedMs,
			"bytes":      len(reply),
		}, task.SpaceID)
}

//nolint:unused
func (d *Daemon) matterWriteFailedActivity(parent context.Context, task MatterBotTask, errMsg string, elapsedMs int64) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	_ = d.client.WriteMatterActivity(ctx, task.MatterID, task.BotUID, "agent_task_failed",
		map[string]any{
			"bot_uid":    task.BotUID,
			"task_id":    task.ID,
			"elapsed_ms": elapsedMs,
			"error":      errMsg,
		}, task.SpaceID)
}

//nolint:unused
func (d *Daemon) ackMatterTask(parent context.Context, task MatterBotTask, status, resultSummary, errMsg string) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	if err := d.client.AckMatterBotTask(ctx, task.ID, task.ClaimToken, status, errMsg, resultSummary, 0); err != nil {
		log.Printf("[ERROR] [matter-task] ack id=%s status=%s failed: %v", task.ID, status, err)
		return
	}
	log.Printf("[INFO] [matter-task] acked id=%s status=%s", task.ID, status)
}

// pollMatterTasksForManagedBots issues a single batched matter pull for
// the entire managed bot list (matter side runs per-bot claim queries
// inside one transaction). Tasks are then grouped by bot_uid and each
// group handled sequentially in its own goroutine so a stuck agent only
// blocks its own bot's queue.
//
// Replaces the prior fan-out (N goroutines × N HTTP calls) — saves both
// HTTP round-trips and matter DB transactions at scale. Falls back to
// per-bot pulls when matter is too old to know the bot_uids form.
//
// Per-bot in-flight gate: when matterPullLoop's ticker fires while a
// bot's previous goroutine is still draining (LLM runs can take minutes),
// that bot is filtered out of the batch this tick. Without the gate,
// parallel ticks would claim more tasks against the same bot before the
// prior batch finished — breaking per-bot ordering and prematurely
// consuming claim_token leases.
//nolint:unused
func (d *Daemon) pollMatterTasksForManagedBots(parent context.Context, managed []ManagedBot) {
	if len(managed) == 0 {
		return
	}
	uids := make([]string, 0, len(managed))
	workspaceByBot := make(map[string]string, len(managed))
	d.mu.Lock()
	for _, mb := range managed {
		if mb.BotUID == "" || d.inFlightBots[mb.BotUID] {
			continue
		}
		uids = append(uids, mb.BotUID)
		workspaceByBot[mb.BotUID] = mb.WorkspaceID
	}
	d.mu.Unlock()
	if len(uids) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	tasks, err := d.client.ListMatterBotTasksBatch(ctx, uids, 5)
	cancel()
	if errors.Is(err, ErrMatterBatchUnsupported) {
		// Pre-batch matter — fan out per bot so deployment order isn't
		// load-bearing. Operator log + degraded path. Fallback has its
		// own per-bot in-flight gate.
		log.Printf("[WARN] [matter-task] matter is pre-batch, falling back to per-bot fan-out (upgrade matter to drop this path)")
		d.pollMatterTasksPerBotFallback(parent, managed)
		return
	}
	if err != nil {
		log.Printf("[WARN] [matter-task] batch list failed bots=%d: %v", len(uids), err)
		return
	}
	if len(tasks) == 0 {
		return
	}

	// Group by bot_uid so each bot's tasks process sequentially without
	// blocking other bots.
	byBot := make(map[string][]MatterBotTask, len(uids))
	for i := range tasks {
		byBot[tasks[i].BotUID] = append(byBot[tasks[i].BotUID], tasks[i])
	}
	log.Printf("[INFO] [matter-task] batch pulled %d task(s) for %d bot(s)", len(tasks), len(byBot))

	// Mark in-flight before spawning goroutines (race-safe). Goroutine
	// defers clear the flag when the per-bot drain finishes so the next
	// matterPullLoop tick can claim again.
	d.mu.Lock()
	for botUID := range byBot {
		d.inFlightBots[botUID] = true
	}
	d.mu.Unlock()

	for botUID, perBotTasks := range byBot {
		botUID, perBotTasks := botUID, perBotTasks
		workspaceID := workspaceByBot[botUID]
		go func() {
			defer func() {
				d.mu.Lock()
				delete(d.inFlightBots, botUID)
				d.mu.Unlock()
			}()
			for i := range perBotTasks {
				d.handleMatterBotTask(parent, workspaceID, perBotTasks[i])
			}
		}()
	}
}

// pollMatterTasksPerBotFallback is the legacy fan-out path, retained only
// for the case where matter is older than the bot_uids batch endpoint.
// One goroutine per bot, one HTTP per bot — preserves correctness at the
// cost of the QPS savings batch was meant to deliver.
//nolint:unused
func (d *Daemon) pollMatterTasksPerBotFallback(parent context.Context, managed []ManagedBot) {
	for _, mb := range managed {
		bot := mb
		d.mu.Lock()
		if d.inFlightBots[bot.BotUID] {
			d.mu.Unlock()
			continue
		}
		d.inFlightBots[bot.BotUID] = true
		d.mu.Unlock()

		go func() {
			defer func() {
				d.mu.Lock()
				delete(d.inFlightBots, bot.BotUID)
				d.mu.Unlock()
			}()
			ctx, cancel := context.WithTimeout(parent, 15*time.Second)
			tasks, err := d.client.ListMatterBotTasks(ctx, bot.BotUID, 5)
			cancel()
			if err != nil {
				log.Printf("[WARN] [matter-task] fallback list failed bot=%s: %v", bot.BotUID, err)
				return
			}
			if len(tasks) == 0 {
				return
			}
			log.Printf("[INFO] [matter-task] fallback pulled %d task(s) bot=%s", len(tasks), bot.BotUID)
			for i := range tasks {
				d.handleMatterBotTask(parent, bot.WorkspaceID, tasks[i])
			}
		}()
	}
}
