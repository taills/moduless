package sdk

import (
	"context"

	pb "github.com/taills/moduleless/proto/tunnel"
	"google.golang.org/grpc"
)

// FileMeta describes a stored file.
type FileMeta struct {
	FileID   string
	Filename string
	Size     int64
	MimeType string
}

// FilesClient lets extensions mint download URLs and read file metadata.
// Binary file bytes never travel through the tunnel — only file ids and tokens.
type FilesClient struct {
	client pb.FileServiceClient
}

func NewFilesClient(conn *grpc.ClientConn) *FilesClient {
	return &FilesClient{client: pb.NewFileServiceClient(conn)}
}

// GenerateDownloadURL returns a clean path-param download URL (no query string).
func (c *FilesClient) GenerateDownloadURL(ctx context.Context, fileID, userID string, expirySeconds int32) (string, error) {
	resp, err := c.client.GenerateDownloadToken(ctx, &pb.TokenRequest{
		FileId:        fileID,
		UserId:        userID,
		ExpirySeconds: expirySeconds,
	})
	if err != nil {
		return "", err
	}
	return resp.Url, nil
}

// GetMetadata fetches stored metadata for a file id.
func (c *FilesClient) GetMetadata(ctx context.Context, fileID string) (*FileMeta, bool, error) {
	resp, err := c.client.GetMetadata(ctx, &pb.FileMetaRequest{FileId: fileID})
	if err != nil {
		return nil, false, err
	}
	if !resp.Found {
		return nil, false, nil
	}
	return &FileMeta{
		FileID:   resp.FileId,
		Filename: resp.Filename,
		Size:     resp.Size,
		MimeType: resp.MimeType,
	}, true, nil
}
