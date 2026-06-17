package internal

import (
	"context"
	"log"
	"time"
)

func (d *Daemon) handleUpgrade(ctx context.Context, up *PendingUpgrade) error {
	switch up.Component {
	case "octo":
		return d.handlePluginUpgrade(ctx, up)
	case "", "octo-daemon":
		return d.handleDaemonUpgrade(ctx, up)
	case "claude", "codex", "openclaw", "hermes":
		return d.handleComponentUpgrade(ctx, up)
	default:
		log.Printf("[ERROR] unsupported upgrade component: %s", up.Component)
		return d.reportUpgrade(ctx, up.TaskID, "failed", "unsupported component: "+up.Component)
	}
}

// handleDaemonUpgrade is intentionally a no-op. The daemon binary is distributed
// via npm (and rolled as a container image under k8s); upgrading it in-process
// would fight npm's package management (it would overwrite the platform
// sub-package file npm owns) and duplicate, more weakly, the arch/os selection
// and integrity checks npm already provides. Daemon upgrades belong to the
// orchestration layer, not the running process.
//
// The task is closed out as a terminal "failed" with a clear reason so a legacy
// server that still dispatches it doesn't leave the task stuck until the
// sweeper times out.
func (d *Daemon) handleDaemonUpgrade(ctx context.Context, up *PendingUpgrade) error {
	const msg = "daemon upgrades are managed via npm/k8s, not supported in-process"
	log.Printf("[INFO] ignoring daemon self-upgrade task %s: %s", up.TaskID, msg)
	return d.reportUpgrade(ctx, up.TaskID, "failed", msg)
}

// reportUpgrade 把 status 发回 fleet. 返回 ReportUpgrade 的实际 err.
//
// caller 决定 swallow vs propagate (Jerry-Xin Critical fix):
//   - progress status (downloading/installing/restarting) — 失败不致命, 用
//     `_ = d.reportUpgrade(...)` swallow. 后续 terminal report 会覆盖.
//   - terminal status (failed) — 失败致命, 必须 `return d.reportUpgrade(...)`
//     往 handler 上抛 → adapter (HandleUpgrade) 透传 → dispatcher 不 markDone
//     → SSE replay / heartbeat 兜底重试. 不传则 fleet 永远不知道 task 终结,
//     daemon 端 dedup 已 markDone, SSE replay/heartbeat 都不会再触发,
//     task 永远卡在 dispatched/installing/... 直到 sweeper timeout 误报.
func (d *Daemon) reportUpgrade(ctx context.Context, taskID, status, errMsg string) error {
	reportCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.client.ReportUpgrade(reportCtx, taskID, status, errMsg); err != nil {
		log.Printf("[WARN] upgrade report failed (status=%s): %v", status, err)
		return err
	}
	log.Printf("[INFO] upgrade status: %s", status)
	return nil
}
