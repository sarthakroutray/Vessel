package storage

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// streamCopyBuf is the size of the stack-allocated copy buffer used by
// io.CopyBuffer when piping decoded bytes to disk.
// 32 KB is the sweet-spot: large enough to amortise syscall overhead across
// big files, small enough to keep per-goroutine stack pressure negligible.
const streamCopyBuf = 32 * 1024 // 32 KB

// SaveAttachmentStream decodes a Base64-encoded payload and writes the raw
// bytes directly to a temporary file on disk — without ever materialising the
// full decoded content in memory.
//
// Memory profile:
//   - base64.NewDecoder wraps a strings.NewReader over the caller's string.
//     The string itself is already in memory (it came from the HTTP body), but
//     the *decoded* bytes are never buffered — they are piped 32 KB at a time
//     straight into the kernel page cache via the file descriptor.
//   - Peak extra allocation is therefore ≈ 32 KB per concurrent attachment,
//     regardless of whether the file is 1 KB or 100 MB.
//
// The caller owns the returned path and is responsible for calling
// storage.PruneFile(path) after the file is no longer needed.
func SaveAttachmentStream(base64Payload string, fileName string) (string, error) {
	// Ensure the scratch directory exists (idempotent).
	dir := filepath.Clean(tmpPrefix)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir attachments: %w", err)
	}

	// UUID-qualify the filename so concurrent saves of identically named files
	// never collide, even across multiple goroutines or worker processes.
	localName := fmt.Sprintf("%s_%s", uuid.New().String(), filepath.Base(fileName))
	localPath := filepath.Join(dir, localName)

	// Open the destination file for writing. O_CREATE|O_WRONLY|O_TRUNC is the
	// minimum permission set; we never need to read back during this function.
	f, err := os.OpenFile(localPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("create attachment file: %w", err)
	}
	// Always close the file descriptor, even if io.CopyBuffer fails mid-stream.
	// If we return an error we also attempt to remove the partial file.
	defer func() {
		f.Close()
		if err != nil {
			// Best-effort cleanup of the partial file on failure.
			_ = os.Remove(localPath)
		}
	}()

	// Chain: base64Payload string → strings.Reader → base64.Decoder → file FD.
	//
	// base64.NewDecoder is a streaming decoder: it reads the next chunk of
	// base64 text from its underlying Reader, decodes it, and surfaces the
	// decoded bytes to whoever is reading the decoder — in our case io.CopyBuffer.
	//
	// base64.StdEncoding handles standard (padded) Base64 as used by most
	// HTTP clients and browser File APIs. Switch to base64.RawStdEncoding if
	// your clients omit padding characters.
	src := base64.NewDecoder(base64.StdEncoding, strings.NewReader(base64Payload))

	// A reusable 32 KB buffer that lives on the heap for the duration of this
	// call only. io.CopyBuffer reuses it across loop iterations, so total
	// allocation stays constant no matter how large the file is.
	buf := make([]byte, streamCopyBuf)

	if _, err = io.CopyBuffer(f, src, buf); err != nil {
		return "", fmt.Errorf("stream decode attachment to disk: %w", err)
	}

	// Explicit sync to the kernel buffer is not strictly necessary here because
	// the file is short-lived (it will be pruned after delivery), but Sync()
	// would be appropriate for durability-critical data.
	return localPath, nil
}
