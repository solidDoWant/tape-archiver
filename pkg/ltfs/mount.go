package ltfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/solidDoWant/tape-archiver/pkg/tape"
)

// mountPollInterval is how often Mount polls for the FUSE mount to come live.
const mountPollInterval = 100 * time.Millisecond

// Mount is a live LTFS FUSE mount of a Volume. The ltfs process is not bound to
// the context passed to Volume.Mount, so a later activity can finalize it with
// Unmount, Kill, or Abort.
type Mount struct {
	device     string
	mountpoint string
	workDir    string

	// mountStart is a strict lower bound on the mtime of any index this cycle
	// captures; ReadIndex uses it to reject leftovers from a prior format of the
	// same barcode (see pickIndexFile).
	mountStart time.Time

	cmd    *exec.Cmd
	stderr *bytes.Buffer

	// detached is set once fusermount -u succeeds; re-running it on a released
	// mountpoint exits non-zero and would fail an otherwise successful retry.
	detached bool

	// done is closed when ltfs exits; waitErr is valid only after done is closed.
	done    chan struct{}
	waitErr error
}

// Mountpoint is the absolute path the LTFS volume is mounted at.
func (m *Mount) Mountpoint() string {
	return m.mountpoint
}

// Mount mounts the volume at mountpoint with the index sync deferred to unmount
// and index capture into workDir enabled (see the package doc).
func (v *Volume) Mount(ctx context.Context, mountpoint, workDir string) (*Mount, error) {
	// Taken first so it is a strict lower bound on any index this cycle writes.
	mountStart := time.Now()

	absMount, err := filepath.Abs(mountpoint)
	if err != nil {
		return nil, fmt.Errorf("resolve mountpoint %q: %w", mountpoint, err)
	}

	// Refuse an already-mounted path (live, or orphaned by a killed daemon) so
	// any mount waitForMount observes is necessarily our own ltfs. Runs before
	// MkdirAll so an orphan's ENOTCONN cannot surface as an opaque stat error.
	inUse, err := mountpointInUse(absMount)
	if err != nil {
		return nil, fmt.Errorf("check whether %s is already mounted: %w", absMount, err)
	}

	if inUse {
		return nil, fmt.Errorf("mountpoint %s is already in use by an existing LTFS mount "+
			"(live from a prior run, or orphaned by a killed daemon); release it first with "+
			"`fusermount -u %s`", absMount, absMount)
	}

	for _, dir := range []string{absMount, workDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	stderr := &bytes.Buffer{}
	// Not CommandContext: ltfs must outlive this activity's context.
	cmd := exec.Command("ltfs", ltfsArgs(v.device, absMount, workDir)...)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ltfs: %w", err)
	}

	m := &Mount{
		device:     v.device,
		mountpoint: absMount,
		workDir:    workDir,
		mountStart: mountStart,
		cmd:        cmd,
		stderr:     stderr,
		done:       make(chan struct{}),
	}

	go func() {
		m.waitErr = cmd.Wait()
		close(m.done)
	}()

	if err := m.waitForMount(ctx); err != nil {
		select {
		case <-m.done:
			// ltfs exited on its own, releasing whatever it held.
			return nil, err
		default:
		}

		// Killed mid-mount, possibly after ltfs locked the drive.
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), AbortTimeout)
		defer cancel()

		return nil, errors.Join(err, m.Abort(abortCtx))
	}

	return m, nil
}

// waitForMount blocks until the FUSE mount is live, ltfs exits early, or ctx is
// done. Attribution to our process rests on Volume.Mount's pre-start check.
func (m *Mount) waitForMount(ctx context.Context) error {
	ticker := time.NewTicker(mountPollInterval)
	defer ticker.Stop()

	for {
		mounted, err := isMountpoint(m.mountpoint)
		if err != nil {
			return fmt.Errorf("check mount status of %s: %w", m.mountpoint, err)
		}

		if mounted {
			return nil
		}

		select {
		case <-m.done:
			// ltfs can exit 0 without mounting; %w of nil would render "%!w(<nil>)".
			if m.waitErr != nil {
				return m.withLTFSOutput(
					fmt.Errorf("ltfs exited before the mount became ready: %w", m.waitErr))
			}

			return m.withLTFSOutput(
				errors.New("ltfs exited with status 0 before the mount became ready"))
		case <-ctx.Done():
			return fmt.Errorf("waiting for ltfs mount at %s: %w", m.mountpoint, ctx.Err())
		case <-ticker.C:
		}
	}
}

// withLTFSOutput appends captured ltfs stderr to err as indented lines so the
// drive's diagnostics reach the operator (SPEC.md §11); every error surface
// renders multi-line.
func (m *Mount) withLTFSOutput(err error) error {
	detail := strings.TrimSpace(m.stderr.String())
	if detail == "" {
		return err
	}

	return fmt.Errorf("%w\nltfs output:\n%s", err, indentLines(detail, "  "))
}

// indentLines prefixes every line of s with prefix.
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}

	return strings.Join(lines, "\n")
}

// Unmount releases the FUSE mount and waits for ltfs to exit, which is when the
// index is written (SPEC.md §14); a non-nil return means the tape is not safely
// written.
//
// fusermount -u returns before LTFS flushes the index (~30ms on mhvtl while the
// daemon kept writing), hence the wait; lazy unmount (-uz) is avoided for the
// same reason. Unmount is idempotent for the FinalizeTape retry loop.
func (m *Mount) Unmount(ctx context.Context) error {
	if !m.detached {
		cmd := exec.CommandContext(ctx, "fusermount", "-u", m.mountpoint)

		if out, err := cmd.CombinedOutput(); err != nil {
			// Benign if ltfs already exited (the mount is gone); otherwise the
			// detach genuinely failed.
			select {
			case <-m.done:
			default:
				if msg := strings.TrimSpace(string(out)); msg != "" {
					return fmt.Errorf("%s: %w: %s", cmd, err, msg)
				}

				return fmt.Errorf("%s: %w", cmd, err)
			}
		}

		m.detached = true
	}

	select {
	case <-m.done:
		if m.waitErr != nil {
			return m.withLTFSOutput(
				fmt.Errorf("ltfs index write at unmount failed: %w", m.waitErr))
		}

		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for ltfs to finish writing the index at unmount: %w", ctx.Err())
	}
}

// Kill forcibly terminates ltfs and waits for it to exit. Unlike Unmount it does
// not guarantee the index was written, so the tape must be re-written (SPEC §14).
// os.ErrProcessDone from an already-exited process is harmless.
func (m *Mount) Kill() error {
	err := m.cmd.Process.Kill()
	<-m.done

	return err
}

// AbortTimeout is a sensible bound for Abort's cleanup (a lazy unmount and one
// SCSI command).
const AbortTimeout = time.Minute

// Abort is Kill plus releasing what the killed ltfs held (#442): it lazily
// detaches the orphaned FUSE mountpoint (fusermount -uz) and clears the drive's
// medium-removal lock, which ltfs sets at mount and clears only on a clean exit;
// left set, the changer refuses to unload the tape. As with Kill, the tape must
// be re-written. Every step runs even if an earlier one fails; errors are
// joined. Pass a live ctx: callers usually reach Abort because theirs expired.
func (m *Mount) Abort(ctx context.Context) error {
	var errs []error

	if err := m.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		errs = append(errs, fmt.Errorf("kill ltfs: %w", err))
	}

	if err := m.detachStale(ctx); err != nil {
		errs = append(errs, err)
	}

	// Safe only now that ltfs is gone: the lock guards a live mount.
	if err := tape.AllowMediumRemoval(ctx, m.device); err != nil {
		errs = append(errs, fmt.Errorf("release drive %s after killing ltfs: %w", m.device, err))
	}

	return errors.Join(errs...)
}

// detachStale lazily unmounts the mountpoint if a killed ltfs left it mounted.
func (m *Mount) detachStale(ctx context.Context) error {
	inUse, err := mountpointInUse(m.mountpoint)
	if err != nil {
		return fmt.Errorf("check for a stale mount at %s: %w", m.mountpoint, err)
	}

	if !inUse {
		return nil
	}

	cmd := exec.CommandContext(ctx, "fusermount", "-uz", m.mountpoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("detach stale mount: %s: %w: %s", cmd, err, msg)
		}

		return fmt.Errorf("detach stale mount: %s: %w", cmd, err)
	}

	m.detached = true

	return nil
}

// ltfsArgs builds the ltfs mount arguments: -f for supervision, a single index
// write at unmount (SPEC.md §14), and index capture for ReadIndex.
func ltfsArgs(device, mountpoint, workDir string) []string {
	return []string{
		"-f",
		mountpoint,
		"-o", "devname=" + device,
		"-o", "sync_type=unmount",
		"-o", "capture_index",
		"-o", "work_directory=" + workDir,
	}
}

// isMountpoint reports whether path's st_dev differs from its parent's.
func isMountpoint(path string) (bool, error) {
	var st, parent syscall.Stat_t

	if err := syscall.Stat(path, &st); err != nil {
		return false, err
	}

	if err := syscall.Stat(filepath.Dir(path), &parent); err != nil {
		return false, err
	}

	return st.Dev != parent.Dev, nil
}

// mountpointInUse reports whether path is listed in /proc/self/mountinfo. Unlike
// isMountpoint it never stats path, so it also detects an orphaned FUSE mount
// (ENOTCONN). Only the parent is canonicalized, to match the kernel's
// symlink-free paths without stat'ing an orphaned final component.
func mountpointInUse(path string) (bool, error) {
	target := path
	if resolved, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		target = filepath.Join(resolved, filepath.Base(path))
	}

	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}

	for _, line := range strings.Split(string(data), "\n") {
		// proc(5): the mount point is the 5th field, octal-escaped.
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		if unescapeMountinfo(fields[4]) == target {
			return true, nil
		}
	}

	return false, nil
}

// unescapeMountinfo decodes mountinfo's \OOO octal escapes (see proc(5)).
func unescapeMountinfo(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}

	var b strings.Builder

	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if n, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))

				i += 3

				continue
			}
		}

		b.WriteByte(field[i])
	}

	return b.String()
}
