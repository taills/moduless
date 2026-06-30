package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	sqlc "github.com/taills/moduless/core/db/sqlc"
	pb "github.com/taills/moduless/proto/tunnel"
)

// FileServer implements the inner FileService gRPC API used by extensions to
// mint download tokens and read file metadata. Binary bytes never cross gRPC.
type FileServer struct {
	pb.UnimplementedFileServiceServer
	Queries *sqlc.Queries
}

func NewFileServer(q *sqlc.Queries) *FileServer {
	return &FileServer{Queries: q}
}

// GenerateDownloadToken issues a short-lived token and the clean path-param URL.
func (s *FileServer) GenerateDownloadToken(ctx context.Context, req *pb.TokenRequest) (*pb.TokenResponse, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(b)

	expiry := req.ExpirySeconds
	if expiry <= 0 {
		expiry = 300 // default 5 minutes
	}
	expiresAt := time.Now().Add(time.Duration(expiry) * time.Second)

	if err := s.Queries.InsertDownloadToken(ctx, sqlc.InsertDownloadTokenParams{
		Token:     token,
		FileID:    req.FileId,
		UserID:    req.UserId,
		ExpiresAt: expiresAt,
	}); err != nil {
		return nil, err
	}

	return &pb.TokenResponse{
		Token: token,
		Url:   "/api/system/files/download/" + req.FileId + "/" + token,
	}, nil
}

// GetMetadata returns stored metadata for a file id.
func (s *FileServer) GetMetadata(ctx context.Context, req *pb.FileMetaRequest) (*pb.FileMetaResponse, error) {
	f, err := s.Queries.GetFile(ctx, req.FileId)
	if err != nil {
		return &pb.FileMetaResponse{Found: false}, nil
	}
	return &pb.FileMetaResponse{
		Found:    true,
		FileId:   f.ID,
		Filename: f.Filename,
		Size:     f.Size,
		MimeType: f.MimeType,
	}, nil
}
