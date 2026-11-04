package main

import "syscall"

// Write from the producer's Go worker without os.File's descriptor-1 EPIPE
// policy, which deliberately raises SIGPIPE and can terminate the host. A raw
// Go syscall returns EPIPE instead. Do not change process signal disposition,
// duplicate/close host-owned stdout, or retry a partial/ambiguous record.
type replayStdoutWriter struct{}

func (replayStdoutWriter) Write(data []byte) (int, error) {
	n, err := syscall.Write(syscall.Stdout, data)
	if n < 0 {
		n = 0
	}
	return n, err
}
