package internal

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (d *Daemon) handleUpgrade(ctx context.Context, up *PendingUpgrade) error {
	switch up.Component {
	case "octo", "cc-octo":
		// Both are octo-adapter "plugins" (openclaw's bundled octo / claude's
		// cc-channel-octo gateway). handlePluginUpgrade branches on the
		// component to pick the install command + liveness probe.
		return d.handlePluginUpgrade(ctx, up)
	case "", "octo-daemon":
		return d.handleDaemonUpgrade(ctx, up)
	case "claude", "openclaw":
		return d.handleComponentUpgrade(ctx, up)
	default:
		log.Printf("[ERROR] unsupported upgrade component: %s", up.Component)
		return d.reportUpgrade(ctx, up.TaskID, "failed", "unsupported component: "+up.Component)
	}
}

func (d *Daemon) handleDaemonUpgrade(ctx context.Context, up *PendingUpgrade) error {
	log.Printf("[INFO] upgrade task received: %s → %s (task=%s)", d.cfg.CLIVersion, up.TargetVersion, up.TaskID)

	// 0. 前置检查：当前二进制路径是否可写
	exePath, err := os.Executable()
	if err != nil {
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("cannot determine executable path: %v", err))
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("cannot resolve symlinks: %v", err))
	}

	if err := checkWritable(exePath); err != nil {
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("install path not writable: %v", err))
	}

	// 1. checksum 校验
	if up.Checksum == "" {
		return d.reportUpgrade(ctx, up.TaskID, "failed", "no checksum provided")
	}

	dataDir := DataDir()
	downloadPath := filepath.Join(dataDir, "upgrade-download.tar.gz")
	tmpDir := filepath.Join(dataDir, "upgrade-tmp")
	bakPath := filepath.Join(dataDir, "octo-daemon.bak")

	// 2. downloading (progress — 失败 swallow, 后续 terminal 会覆盖)
	_ = d.reportUpgrade(ctx, up.TaskID, "downloading", "")
	log.Printf("[INFO] downloading %s", up.DownloadURL)

	if err := downloadFile(ctx, up.DownloadURL, downloadPath); err != nil {
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("download failed: %v", err))
	}

	// 3. SHA256 校验
	actualChecksum, err := sha256File(downloadPath)
	if err != nil {
		cleanup(downloadPath, tmpDir)
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("checksum calculation failed: %v", err))
	}
	expectedChecksum := strings.TrimPrefix(up.Checksum, "sha256:")
	if actualChecksum != expectedChecksum {
		cleanup(downloadPath, tmpDir)
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum))
	}
	log.Printf("[INFO] checksum verified")

	// 4. 解压
	_ = os.MkdirAll(tmpDir, 0755)
	extractedBinary, err := extractTarGz(downloadPath, tmpDir)
	if err != nil {
		cleanup(downloadPath, tmpDir)
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("extract failed: %v", err))
	}
	log.Printf("[INFO] extracted: %s", extractedBinary)

	// 5. installing (progress — 失败 swallow)
	_ = d.reportUpgrade(ctx, up.TaskID, "installing", "")

	// 6. 移到目标同目录
	exeDir := filepath.Dir(exePath)
	newPath := filepath.Join(exeDir, "octo-daemon.new")
	if err := copyFile(extractedBinary, newPath); err != nil {
		cleanup(downloadPath, tmpDir)
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("copy to target dir failed: %v", err))
	}
	// chmod must succeed: a non-executable new binary would make the post-upgrade
	// respawn fail with "permission denied" and silently strand the daemon on the
	// old version. Fail the upgrade loudly instead.
	if err := os.Chmod(newPath, 0755); err != nil {
		cleanup(downloadPath, tmpDir)
		_ = os.Remove(newPath)
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("chmod new binary: %v", err))
	}

	// 7. 备份当前二进制
	if err := copyFile(exePath, bakPath); err != nil {
		cleanup(downloadPath, tmpDir)
		_ = os.Remove(newPath)
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("backup failed: %v", err))
	}
	log.Printf("[INFO] backed up current binary to %s", bakPath)

	// 8. 原子替换（同目录 rename）
	if err := os.Rename(newPath, exePath); err != nil {
		cleanup(downloadPath, tmpDir)
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("replace failed: %v", err))
	}
	log.Printf("[INFO] binary replaced")

	// 9. 清理临时文件
	cleanup(downloadPath, tmpDir)

	// 10. restarting (progress — 失败 swallow, 新 daemon 起来 register 关单)
	_ = d.reportUpgrade(ctx, up.TaskID, "restarting", "")

	// 11. 分两种重启路径：
	//   - 在 service manager 下运行：exit 75 让 launchd/systemd 拉起新二进制。
	//     从 daemon 内部调 systemctl restart 会被 cgroup stop 连累，launchctl
	//     kickstart 时序也不稳。exit-code 驱动更可靠（见 plan §五）。
	//   - 非 service：保留原 shell 脚本路径，用户手工 start 也能自恢复。
	if os.Getenv("OCTO_DAEMON_UNDER_SERVICE") == "1" {
		log.Printf("[INFO] under service manager, exiting 75 to request respawn")
		d.requestExit(&ExitError{Code: 75, Message: "upgrade respawn"})
		return nil
	}

	// 11b (legacy path). fork 一个 shell 脚本等旧进程退出后再启动新二进制。
	configPath := ConfigFilePath()
	pid := os.Getpid()
	lockPath := LockFilePath()
	script := fmt.Sprintf(`
		for i in $(seq 1 20); do
			kill -0 %d 2>/dev/null || break
			sleep 0.5
		done
		# 确认锁文件释放
		for i in $(seq 1 10); do
			if ! flock -n "%s" true 2>/dev/null; then
				sleep 0.5
			else
				break
			fi
		done
		exec "%s" start --config "%s"
	`, pid, lockPath, exePath, configPath)

	restartCmd := exec.Command("sh", "-c", script)
	restartCmd.Stdout = os.Stdout
	restartCmd.Stderr = os.Stderr
	restartCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := restartCmd.Start(); err != nil {
		log.Printf("[ERROR] failed to start restart script: %v", err)
		return d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("restart script failed: %v", err))
	}
	log.Printf("[INFO] restart script launched (pid=%d), shutting down old process...", restartCmd.Process.Pid)

	// 12. 正常退出（走 defer 清理路径：释放锁、删 PID）
	d.cancel()
	return nil
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

func downloadFile(ctx context.Context, url, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(f, resp.Body)
	return err
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func extractTarGz(archive, destDir string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer func() { _ = gzr.Close() }()

	tr := tar.NewReader(gzr)
	var binaryPath string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Base(hdr.Name)
		if strings.HasPrefix(name, "._") {
			continue
		}
		if strings.Contains(name, "octo-daemon") || strings.Contains(name, "octo_daemon") {
			dest := filepath.Join(destDir, name)
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return "", err
			}
			_ = out.Close()
			binaryPath = dest
			break
		}
	}
	if binaryPath == "" {
		return "", fmt.Errorf("no octo-daemon binary found in archive")
	}
	return binaryPath, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

func checkWritable(path string) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, ".octo-write-test")
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("%s: %w", dir, err)
	}
	_ = f.Close()
	_ = os.Remove(tmp)
	return nil
}

func cleanup(paths ...string) {
	for _, p := range paths {
		_ = os.RemoveAll(p)
	}
}
