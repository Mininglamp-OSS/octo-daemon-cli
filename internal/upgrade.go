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

func (d *Daemon) handleUpgrade(ctx context.Context, up *PendingUpgrade) {
	switch up.Component {
	case "openclaw-channel-dmwork":
		d.handlePluginUpgrade(ctx, up)
	case "", "octo-daemon":
		d.handleDaemonUpgrade(ctx, up)
	case "claude", "codex", "openclaw", "hermes":
		d.handleComponentUpgrade(ctx, up)
	default:
		log.Printf("[ERROR] unsupported upgrade component: %s", up.Component)
		d.reportUpgrade(ctx, up.TaskID, "failed", "unsupported component: "+up.Component)
	}
}

func (d *Daemon) handleDaemonUpgrade(ctx context.Context, up *PendingUpgrade) {
	log.Printf("[INFO] upgrade task received: %s → %s (task=%s)", d.cfg.CLIVersion, up.TargetVersion, up.TaskID)

	// 0. 前置检查：当前二进制路径是否可写
	exePath, err := os.Executable()
	if err != nil {
		d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("cannot determine executable path: %v", err))
		return
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("cannot resolve symlinks: %v", err))
		return
	}

	if err := checkWritable(exePath); err != nil {
		d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("install path not writable: %v", err))
		return
	}

	// 1. checksum 校验
	if up.Checksum == "" {
		d.reportUpgrade(ctx, up.TaskID, "failed", "no checksum provided")
		return
	}

	dataDir := DataDir()
	downloadPath := filepath.Join(dataDir, "upgrade-download.tar.gz")
	tmpDir := filepath.Join(dataDir, "upgrade-tmp")
	bakPath := filepath.Join(dataDir, "octo-daemon.bak")

	// 2. downloading
	d.reportUpgrade(ctx, up.TaskID, "downloading", "")
	log.Printf("[INFO] downloading %s", up.DownloadURL)

	if err := downloadFile(ctx, up.DownloadURL, downloadPath); err != nil {
		d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("download failed: %v", err))
		return
	}

	// 3. SHA256 校验
	actualChecksum, err := sha256File(downloadPath)
	if err != nil {
		d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("checksum calculation failed: %v", err))
		cleanup(downloadPath, tmpDir)
		return
	}
	expectedChecksum := strings.TrimPrefix(up.Checksum, "sha256:")
	if actualChecksum != expectedChecksum {
		d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum))
		cleanup(downloadPath, tmpDir)
		return
	}
	log.Printf("[INFO] checksum verified")

	// 4. 解压
	_ = os.MkdirAll(tmpDir, 0755)
	extractedBinary, err := extractTarGz(downloadPath, tmpDir)
	if err != nil {
		d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("extract failed: %v", err))
		cleanup(downloadPath, tmpDir)
		return
	}
	log.Printf("[INFO] extracted: %s", extractedBinary)

	// 5. installing
	d.reportUpgrade(ctx, up.TaskID, "installing", "")

	// 6. 移到目标同目录
	exeDir := filepath.Dir(exePath)
	newPath := filepath.Join(exeDir, "octo-daemon.new")
	if err := copyFile(extractedBinary, newPath); err != nil {
		d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("copy to target dir failed: %v", err))
		cleanup(downloadPath, tmpDir)
		return
	}
	_ = os.Chmod(newPath, 0755)

	// 7. 备份当前二进制
	if err := copyFile(exePath, bakPath); err != nil {
		d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("backup failed: %v", err))
		cleanup(downloadPath, tmpDir)
		_ = os.Remove(newPath)
		return
	}
	log.Printf("[INFO] backed up current binary to %s", bakPath)

	// 8. 原子替换（同目录 rename）
	if err := os.Rename(newPath, exePath); err != nil {
		d.reportUpgrade(ctx, up.TaskID, "failed", fmt.Sprintf("replace failed: %v", err))
		cleanup(downloadPath, tmpDir)
		return
	}
	log.Printf("[INFO] binary replaced")

	// 9. 清理临时文件
	cleanup(downloadPath, tmpDir)

	// 10. restarting
	d.reportUpgrade(ctx, up.TaskID, "restarting", "")

	// 11. 分两种重启路径：
	//   - 在 service manager 下运行：exit 75 让 launchd/systemd 拉起新二进制。
	//     从 daemon 内部调 systemctl restart 会被 cgroup stop 连累，launchctl
	//     kickstart 时序也不稳。exit-code 驱动更可靠（见 plan §五）。
	//   - 非 service：保留原 shell 脚本路径，用户手工 start 也能自恢复。
	if os.Getenv("OCTO_DAEMON_UNDER_SERVICE") == "1" {
		log.Printf("[INFO] under service manager, exiting 75 to request respawn")
		d.requestExit(&ExitError{Code: 75, Message: "upgrade respawn"})
		return
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
		return
	}
	log.Printf("[INFO] restart script launched (pid=%d), shutting down old process...", restartCmd.Process.Pid)

	// 12. 正常退出（走 defer 清理路径：释放锁、删 PID）
	d.cancel()
}

func (d *Daemon) reportUpgrade(ctx context.Context, taskID, status, errMsg string) {
	reportCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.client.ReportUpgrade(reportCtx, taskID, status, errMsg); err != nil {
		log.Printf("[WARN] upgrade report failed (status=%s): %v", status, err)
	} else {
		log.Printf("[INFO] upgrade status: %s", status)
	}
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
	defer f.Close()
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
	defer f.Close()

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
