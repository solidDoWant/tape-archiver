//go:build linux

package mhvtl

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

// mhvtl's daemons take operator commands over one SysV message queue; this
// speaks the protocol vtlcmd does (include/utils/q.h). vtlcmd itself is not used
// because its "load map" pre-check looks for a media file that current mktape no
// longer creates.
//
// The queue lives in the host IPC namespace, so the worker's container needs
// --ipc=host.
const (
	queueKey     = 0x4d61726b // QKEY
	commandMType = 32768      // VTLCMD_Q: the sender ID replies are addressed to
	maxTextLen   = 1024       // MAXTEXTLEN

	ipcNoWait  = 0o4000  // IPC_NOWAIT
	msgNoError = 0o10000 // MSG_NOERROR

	replyTimeout = 30 * time.Second
	replyPoll    = 20 * time.Millisecond
)

// queueEntry is struct q_entry: the message type, then struct q_msg.
type queueEntry struct {
	mtype  int64
	sender int64
	text   [maxTextLen + 1]byte
}

// payloadSize is sizeof(struct q_msg), the msgsz both ends use.
const payloadSize = int(unsafe.Sizeof(queueEntry{})) - 8

func sendCommand(ctx context.Context, libraryID int, cmd string) (string, error) {
	if len(cmd) > maxTextLen {
		return "", fmt.Errorf("command longer than %d bytes", maxTextLen)
	}

	qid, _, errno := syscall.Syscall(syscall.SYS_MSGGET, queueKey, 0, 0)
	if errno != 0 {
		return "", fmt.Errorf("open the mhvtl message queue: %w (is mhvtl running, and does this process share the host IPC namespace?)", errno)
	}

	// Discard replies left by an earlier caller that gave up waiting, so they
	// are not mistaken for the answer to this command.
	for {
		var stale queueEntry
		if _, err := receive(qid, &stale); err != nil {
			break
		}
	}

	request := queueEntry{mtype: int64(libraryID), sender: commandMType}
	copy(request.text[:], cmd)

	size := 8 + len(cmd) + 1
	if _, _, errno := syscall.Syscall6(syscall.SYS_MSGSND, qid, uintptr(unsafe.Pointer(&request)),
		uintptr(size), 0, 0, 0); errno != 0 {
		return "", fmt.Errorf("send to the mhvtl message queue: %w", errno)
	}

	deadline := time.Now().Add(replyTimeout)

	for {
		var reply queueEntry

		n, err := receive(qid, &reply)
		if err == nil {
			text := reply.text[:max(0, min(n-8, len(reply.text)))]
			if end := indexNUL(text); end >= 0 {
				text = text[:end]
			}

			return string(text), nil
		}

		if !errors.Is(err, syscall.ENOMSG) {
			return "", fmt.Errorf("receive from the mhvtl message queue: %w", err)
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("no reply from mhvtl library %d within %s", libraryID, replyTimeout)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(replyPoll):
		}
	}
}

// receive takes one reply addressed to commandMType without blocking, returning
// the payload length.
func receive(qid uintptr, entry *queueEntry) (int, error) {
	n, _, errno := syscall.Syscall6(syscall.SYS_MSGRCV, qid, uintptr(unsafe.Pointer(entry)),
		uintptr(payloadSize), commandMType, ipcNoWait|msgNoError, 0)
	if errno != 0 {
		return 0, errno
	}

	return int(n), nil
}

func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}

	return -1
}
