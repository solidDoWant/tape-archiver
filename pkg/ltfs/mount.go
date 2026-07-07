package ltfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// mountPollInterval is how often Mount polls for the FUSE mount to come live.
const mountPollInterval = 100 * time.Millisecond

// Mount is a live LTFS FUSE mount of a Volume. Files written under its
// Mountpoint persist on tape. Its lifecycle is managed explicitly: Unmount
// releases the mount and waits for the ltfs process to exit (confirming the
// single deferred index write completed), and Kill forcibly terminates it.
// The ltfs process is NOT bound to the context passed to Volume.Mount — that
// context only controls the mount wait. This allows the mount to be parked in
// a registry and finalized by a later activity (see pkg workflows/backup).
type Mount struct {
	mountpoint string
	workDir    string

	// mountStart is a strict lower bound on the mtime of any index this mount
	// cycle could capture: it is set before the ltfs process starts, so a
	// captured index written at unmount always postdates it, while a leftover
	// index from a prior format of the same barcode always predates it.
	// ReadIndex uses it to reject stale leftovers (see pickIndexFile).
	mountStart time.Time

	cmd    *exec.Cmd
	stderr *bytes.Buffer

	// detached is true once fusermount -u has successfully released the FUSE
	// mount. A subsequent Unmount skips the detach: re-running fusermount -u on an
	// already-released mountpoint exits non-zero, which must not fail an otherwise
	// successful retry (see Unmount).
	detached bool

	// done is closed when the supervised ltfs process exits; waitErr holds its
	// exit error and is safe to read only after done is closed.
	done    chan struct{}
	waitErr error
}

// Mountpoint is the absolute path the LTFS volume is mounted at.
func (m *Mount) Mountpoint() string {
	return m.mountpoint
}

// Mount mounts the volume as a FUSE filesystem at mountpoint, with the index
// sync deferred to unmount and index capture enabled (see the package doc).
// workDir is LTFS's work directory; the captured index XML is written there at
// unmount and read back by ReadIndex. Both directories are created if missing.
//
// The ltfs process is run in the foreground and supervised so that Unmount can
// wait for it and learn whether the index write succeeded.
func (v *Volume) Mount(ctx context.Context, mountpoint, workDir string) (*Mount, error) {
	// Capture the mount start before anything else so it is a strict lower bound
	// on any index this cycle writes; ReadIndex rejects captured files older than
	// this as leftovers from a prior format (see Mount.mountStart, pickIndexFile).
	mountStart := time.Now()

	absMount, err := filepath.Abs(mountpoint)
	if err != nil {
		return nil, fmt.Errorf("resolve mountpoint %q: %w", mountpoint, err)
	}

	// Establish attribution before spawning ltfs. Mountpoints are stable per
	// barcode, so the path may already carry a mount — a live one from a prior
	// run, or one orphaned by a killed daemon (stat returns ENOTCONN but it is
	// still listed in /proc/self/mountinfo). Refuse rather than mount over it:
	// because the path is proven un-mounted here, any mount waitForMount later
	// observes is necessarily from our own ltfs. This also runs before MkdirAll
	// so an orphaned mount's ENOTCONN cannot surface as an opaque stat error.
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
	// exec.Command (not CommandContext) so the ltfs process is NOT killed when
	// ctx is cancelled — the process must survive past the launching activity's
	// context so a later activity can finalize it via Unmount or Kill.
	cmd := exec.Command("ltfs", ltfsArgs(v.device, absMount, workDir)...)
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ltfs: %w", err)
	}

	m := &Mount{
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
		// The mount never came live; tear down the process so we don't leak it.
		_ = cmd.Process.Kill()

		<-m.done

		return nil, err
	}

	return m, nil
}

// waitForMount blocks until the FUSE mount is live, the ltfs process exits early
// (a mount failure), or ctx is done.
//
// isMountpoint is a pure st_dev comparison and cannot by itself attribute an
// observed mount to m.cmd. Attribution instead rests on the pre-start invariant
// established by Volume.Mount: the mountpoint is proven un-mounted before ltfs is
// spawned, so any mount observed here is necessarily from our own ltfs process.
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
			return fmt.Errorf("ltfs exited before the mount became ready: %w: %s",
				m.waitErr, strings.TrimSpace(m.stderr.String()))
		case <-ctx.Done():
			return fmt.Errorf("waiting for ltfs mount at %s: %w", m.mountpoint, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Unmount releases the FUSE mount and waits for the LTFS index to be written.
//
// fusermount -u returns as soon as the mount is detached; it does NOT wait for
// LTFS to flush the index to tape (validated against mhvtl: it returned in
// ~30ms while the daemon kept writing). So checking only fusermount's exit
// status would report success before the index is on tape. Instead, after
// detaching, Unmount waits for the supervised foreground ltfs process to exit —
// the index is written exactly once, at that point (SPEC.md §14), and the
// process exit status reflects whether the write succeeded. A non-nil return
// therefore means the Write phase must not treat the tape as safely written.
//
// Lazy unmount (fusermount -uz) is deliberately not used: it detaches without
// ensuring the flush, defeating this guarantee.
//
// Unmount is idempotent, which the FinalizeTape retry loop relies on: if a first
// attempt detaches the mount but its index-write wait returns a context error,
// re-running fusermount -u on the now-detached mountpoint would exit non-zero
// ("entry ... not found in /etc/mtab") and spuriously fail a tape whose index is
// already on disk. A retry therefore skips the detach once it has succeeded, and
// reports the recorded index-write result instead.
func (m *Mount) Unmount(ctx context.Context) error {
	// Detach the FUSE mount, unless a prior Unmount already did. Skipping the
	// re-detach is what makes a retry succeed once the index write has completed.
	if !m.detached {
		cmd := exec.CommandContext(ctx, "fusermount", "-u", m.mountpoint)

		if out, err := cmd.CombinedOutput(); err != nil {
			// A fusermount failure is benign if the supervised ltfs process has
			// already exited: the mount is already gone (a prior detach, or an
			// external kill/crash), so fall through to report the index-write
			// result. If the process is still live the detach genuinely failed,
			// and the error stands.
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
			return fmt.Errorf("ltfs index write at unmount failed: %w: %s",
				m.waitErr, strings.TrimSpace(m.stderr.String()))
		}

		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for ltfs to finish writing the index at unmount: %w", ctx.Err())
	}
}

// Kill forcibly terminates the supervised ltfs process and waits for it to
// exit. It is called by the mount registry teardown when Unmount cannot run
// (e.g. the workflow was cancelled before Finalize completed). Unlike Unmount,
// Kill does not guarantee the LTFS index was written — the tape must be
// re-written on the next run (SPEC §14).
//
// Kill returns the error from os.Process.Kill. The caller may see
// os.ErrProcessDone when the process has already exited on its own, which is
// harmless — <-m.done returns immediately from the already-closed channel
// regardless. Any other non-nil error means the signal could not be delivered,
// and <-m.done may block until the process exits by other means.
func (m *Mount) Kill() error {
	err := m.cmd.Process.Kill()
	<-m.done

	return err
}

// ltfsArgs builds the ltfs mount argument list. -f keeps ltfs in the foreground
// so the process can be supervised (see Mount/Unmount); sync_type=unmount defers
// the index write to a single write at unmount (SPEC.md §14); capture_index
// dumps that index to the work directory for ReadIndex.
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

// isMountpoint reports whether path is a mount point, by comparing its device
// number to that of its parent: a mounted filesystem changes st_dev at the mount
// boundary. This assumes path's parent lives on a different mount than path only
// once path is mounted, which holds for the directories Mount creates.
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

// mountpointInUse reports whether path is currently a mount point, by scanning
// /proc/self/mountinfo. Unlike isMountpoint (a stat-based st_dev comparison), it
// does not stat path, so it detects an orphaned FUSE mount whose stat returns
// ENOTCONN just as well as a live mount — both remain listed in mountinfo.
//
// The query path is canonicalized through its parent directory so a symlinked
// ancestor still matches the kernel's symlink-free mountinfo path; resolving the
// parent (a normal directory) rather than path itself avoids stat'ing an
// orphaned final component.
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
		// Per proc(5), the mount point is the 5th space-separated field, with
		// space, tab, newline, and backslash octal-escaped.
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

// unescapeMountinfo decodes the octal escaping (\OOO) that /proc/self/mountinfo
// applies to space, tab, newline, and backslash in its path fields (see proc(5)).
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
