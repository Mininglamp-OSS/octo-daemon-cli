// PR-B.2 matter-driven task execution. Daemon pulls tasks from matter
// (instead of receiving them via fleet heartbeat) and writes back the
// reply, activity, and ack all directly to matter.
package internal

import (
	"context"
	"log"
	"strings"
	"time"
)

// handleMatterBotTask runs the agent for a matter-pulled task and posts
// the reply + activity + ack back to matter directly.
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
	result, err := runOpenclawAgent(parent, workspaceID, task.Prompt)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		log.Printf("[ERROR] [matter-task] id=%s openclaw agent failed: %v", task.ID, err)
		d.matterWriteFailedActivity(parent, task, err.Error(), elapsed)
		d.ackMatterTask(parent, task, "failed", "", err.Error())
		return
	}
	reply := truncateOutput(result, maxResultSummaryBytes)
	if err := d.matterWriteReplyAndActivity(parent, task, reply, elapsed); err != nil {
		log.Printf("[WARN] [matter-task] id=%s writeback failed: %v (ack still attempted)", task.ID, err)
	}
	log.Printf("[INFO] [matter-task] id=%s succeeded, %d bytes reply, %dms", task.ID, len(reply), elapsed)
	d.ackMatterTask(parent, task, "succeeded", reply, "")
}

func (d *Daemon) matterWriteReplyAndActivity(parent context.Context, task MatterBotTask, reply string, elapsedMs int64) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	// task.ID + task.ClaimToken let matter bind this writeback to the in-flight
	// bot_task we just claimed — without them matter 403s on the JWT path
	// (writeback context check). They're a no-op on legacy X-Internal-Token.
	if err := d.client.WriteMatterTimeline(ctx, task.MatterID, task.BotUID, task.SpaceID, reply, task.ID, task.ClaimToken); err != nil {
		return err
	}
	return d.client.WriteMatterActivity(ctx, task.MatterID, task.BotUID, "agent_task_completed",
		map[string]any{
			"bot_uid":    task.BotUID,
			"task_id":    task.ID,
			"elapsed_ms": elapsedMs,
			"bytes":      len(reply),
		}, task.SpaceID, task.ID, task.ClaimToken)
}

func (d *Daemon) matterWriteFailedActivity(parent context.Context, task MatterBotTask, errMsg string, elapsedMs int64) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	_ = d.client.WriteMatterActivity(ctx, task.MatterID, task.BotUID, "agent_task_failed",
		map[string]any{
			"bot_uid":    task.BotUID,
			"task_id":    task.ID,
			"elapsed_ms": elapsedMs,
			"error":      errMsg,
		}, task.SpaceID, task.ID, task.ClaimToken)
}

func (d *Daemon) ackMatterTask(parent context.Context, task MatterBotTask, status, resultSummary, errMsg string) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	if err := d.client.AckMatterBotTask(ctx, task.ID, task.ClaimToken, status, errMsg, resultSummary, 0); err != nil {
		log.Printf("[ERROR] [matter-task] ack id=%s status=%s failed: %v", task.ID, status, err)
		return
	}
	log.Printf("[INFO] [matter-task] acked id=%s status=%s", task.ID, status)
}

// pollMatterTasksForManagedBots iterates the managed bot list returned
// by fleet's heartbeat and pulls + processes any queued matter tasks
// for each bot. Each bot's tasks are processed sequentially in a
// goroutine so a stuck agent doesn't block others.
func (d *Daemon) pollMatterTasksForManagedBots(parent context.Context, managed []ManagedBot) {
	for _, mb := range managed {
		bot := mb
		go func() {
			ctx, cancel := context.WithTimeout(parent, 15*time.Second)
			tasks, err := d.client.ListMatterBotTasks(ctx, bot.BotUID, 5)
			cancel()
			if err != nil {
				log.Printf("[WARN] [matter-task] list failed bot=%s: %v", bot.BotUID, err)
				return
			}
			if len(tasks) == 0 {
				return
			}
			log.Printf("[INFO] [matter-task] bot=%s pulled %d task(s) from matter", bot.BotUID, len(tasks))
			for i := range tasks {
				d.handleMatterBotTask(parent, bot.WorkspaceID, tasks[i])
			}
		}()
	}
}
