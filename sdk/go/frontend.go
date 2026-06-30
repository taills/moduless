package sdk

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	pb "github.com/taills/moduless/proto/tunnel"
)

// frontendChunkSize bounds each FileChunk payload well under gRPC's 4MB
// default message limit.
const frontendChunkSize = 256 * 1024

// buildFrontendZip walks dir and produces an in-memory zip whose entry names
// are slash-separated paths relative to dir (e.g. "index.html",
// "assets/app.js"). Core extracts these into its micro-frontend cache keyed by
// "/"+entry, which the gateway serves at /extensions/<key>/<entry>. It returns
// the zip bytes and their hex SHA-256 so Core can verify the upload.
func buildFrontendZip(dir string) ([]byte, string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, "", fmt.Errorf("frontend dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("frontend path %q is not a directory", dir)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := io.Copy(w, f); err != nil {
			return err
		}
		return nil
	})
	if walkErr != nil {
		return nil, "", fmt.Errorf("zip frontend %q: %w", dir, walkErr)
	}
	if err := zw.Close(); err != nil {
		return nil, "", fmt.Errorf("finalize frontend zip: %w", err)
	}

	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:]), nil
}

// uploadFrontendZip streams the bundled zip to Core as FileChunk messages and
// signals RegisterComplete so Core extracts it and replies with the result.
func uploadFrontendZip(cs *clientStream, data []byte) error {
	for i := 0; i < len(data); i += frontendChunkSize {
		end := i + frontendChunkSize
		if end > len(data) {
			end = len(data)
		}
		if err := cs.Send(&pb.TunnelMessage{
			Payload: &pb.TunnelMessage_FileChunk{
				FileChunk: &pb.FileChunk{
					Content:    data[i:end],
					ChunkIndex: uint32(i / frontendChunkSize),
				},
			},
		}); err != nil {
			return fmt.Errorf("send frontend chunk: %w", err)
		}
	}
	if err := cs.Send(&pb.TunnelMessage{
		Payload: &pb.TunnelMessage_RegisterComplete{RegisterComplete: &pb.RegisterComplete{}},
	}); err != nil {
		return fmt.Errorf("send register complete: %w", err)
	}
	return nil
}
