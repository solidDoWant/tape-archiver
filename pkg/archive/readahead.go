package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sync/semaphore"
)

// readAhead bounds the small-file read-ahead. On spinning disks each small
// file is several seek-bound I/Os; reading many ahead in parallel keeps the
// pool's queue deep while the tar stream stays in walk order.
type readAhead struct {
	workers int   // concurrent file reads; 0 disables read-ahead (serial path)
	maxFile int64 // files smaller than this are read ahead; larger ones stream from their fd
	memory  int64 // total bytes buffered ahead; larger files are not read ahead
	window  int   // walk entries queued ahead of the writer
}

var defaultReadAhead = readAhead{
	workers: 32,
	maxFile: 1 << 20,
	memory:  64 << 20,
	window:  1024,
}

// prefetch is one small file's open, sparse probe, and read, done ahead of the
// writer. It records each step's error so the writer reproduces the serial
// path's output and errors exactly.
type prefetch struct {
	done     chan struct{}
	weight   int64
	data     []byte
	sparse   bool     // file was handed back; it holds a worker slot until release
	file     *os.File // a sparse file, handed back open for writeSparseEntry
	regions  []sparseRegion
	openErr  error
	probeErr error
	readErr  error
	closeErr error
}

func (p *prefetch) load(ctx context.Context, path string, size int64) {
	defer close(p.done)

	if err := ctx.Err(); err != nil {
		p.openErr = err

		return
	}

	file, err := os.Open(path)
	if err != nil {
		p.openErr = err

		return
	}

	regions, sparse, err := sparseDataRegions(file, size)
	if err != nil {
		p.probeErr = err
		_ = file.Close()

		return
	}

	if sparse {
		p.sparse, p.file, p.regions = true, file, regions

		return
	}

	// The extra byte makes a grown file fail with tar's "write too long", as
	// the serial path does.
	buf := make([]byte, size+1)

	n, err := io.ReadFull(file, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = nil
	}

	p.data, p.readErr = buf[:n], err
	p.closeErr = file.Close()
}

// release frees the buffer, any unconsumed fd, and a handed-back file's worker
// slot once the load finishes.
func (p *prefetch) release(memory, workers *semaphore.Weighted) {
	<-p.done

	if p.file != nil {
		_ = p.file.Close()
		p.file = nil
	}

	if p.sparse {
		workers.Release(1)
	}

	p.data = nil
	memory.Release(p.weight)
}

func (w *tarWriter) writePrefetched(ctx context.Context, header *tar.Header, path string, pre *prefetch) error {
	<-pre.done

	switch {
	case pre.openErr != nil:
		return pre.openErr
	case pre.probeErr != nil:
		return fmt.Errorf("probe %s: %w", path, pre.probeErr)
	case pre.file != nil:
		file := pre.file
		pre.file = nil

		return w.writeOpenFile(ctx, header, path, file, pre.regions, true)
	}

	var body io.Reader = bytes.NewReader(pre.data)
	if pre.readErr != nil {
		body = io.MultiReader(body, errReader{err: pre.readErr})
	}

	if err := w.writeStream(ctx, header, path, body); err != nil {
		return err
	}

	if pre.closeErr != nil {
		return fmt.Errorf("close %s: %w", path, pre.closeErr)
	}

	return nil
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// queuedEntry is a walk entry waiting for the writer; err ends the walk.
type queuedEntry struct {
	walkEntry
	prefetch *prefetch
	err      error
}

func (q queuedEntry) release(memory, workers *semaphore.Weighted) {
	if q.prefetch != nil {
		q.prefetch.release(memory, workers)
	}
}

// treeReadAhead is tree with the walk and small-file reads running ahead of
// the writer, which still consumes entries strictly in walk order.
func (w *tarWriter) treeReadAhead(ctx context.Context, srcDir, prefix string) error {
	config := w.readAhead

	walkCtx, cancel := context.WithCancel(ctx)
	queue := make(chan queuedEntry, config.window)
	memory := semaphore.NewWeighted(config.memory)
	workers := semaphore.NewWeighted(int64(config.workers))

	var loads sync.WaitGroup

	send := func(queued queuedEntry) error {
		select {
		case queue <- queued:
			return nil
		case <-walkCtx.Done():
			queued.release(memory, workers)

			return walkCtx.Err()
		}
	}

	go func() {
		defer close(queue)

		err := walkTree(walkCtx, srcDir, prefix, func(item walkEntry) error {
			queued := queuedEntry{walkEntry: item}

			if config.eligible(item) {
				pre, err := startPrefetch(walkCtx, item, memory, workers, &loads)
				if err != nil {
					return err
				}

				queued.prefetch = pre
			}

			return send(queued)
		})
		if err != nil {
			_ = send(queuedEntry{err: err})
		}
	}()

	// Stop the walk, free whatever is still queued, and wait for every load
	// before returning so no goroutine or fd outlives the call.
	defer func() {
		cancel()

		for queued := range queue {
			queued.release(memory, workers)
		}

		loads.Wait()
	}()

	for queued := range queue {
		if queued.err != nil {
			return queued.err
		}

		if err := ctx.Err(); err != nil {
			queued.release(memory, workers)

			return err
		}

		err := w.writeEntry(ctx, srcDir, prefix, queued.walkEntry, queued.prefetch)
		queued.release(memory, workers)

		if err != nil {
			return err
		}
	}

	// The walk only ends without an error entry if it finished or ctx ended.
	return ctx.Err()
}

// eligible reports whether item is a small regular file worth reading ahead.
// Hardlinks are left to the writer, which may store them as link entries.
func (r readAhead) eligible(item walkEntry) bool {
	if item.infoErr != nil || !item.info.Mode().IsRegular() || item.info.Size() >= min(r.maxFile, r.memory) {
		return false
	}

	_, linked := hardlinkID(item.info)

	return !linked
}

// startPrefetch reserves memory and a worker in walk order, then loads item in
// the background. Reserving in order guarantees the writer's next entry always
// fits, so the budget cannot deadlock. A handed-back sparse file keeps its
// worker slot until released, so open fds stay bounded by workers.
func startPrefetch(ctx context.Context, item walkEntry, memory, workers *semaphore.Weighted, loads *sync.WaitGroup) (*prefetch, error) {
	size := item.info.Size()
	weight := size + 1

	if err := memory.Acquire(ctx, weight); err != nil {
		return nil, err
	}

	if err := workers.Acquire(ctx, 1); err != nil {
		memory.Release(weight)

		return nil, err
	}

	pre := &prefetch{done: make(chan struct{}), weight: weight}

	loads.Go(func() {
		pre.load(ctx, item.path, size)

		if !pre.sparse {
			workers.Release(1)
		}
	})

	return pre, nil
}
