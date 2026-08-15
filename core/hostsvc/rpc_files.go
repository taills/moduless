package hostsvc

import (
	"bytes"
	"context"
	"io"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/taills/moduless/proto/plugin"
)

// PutFile receives a file a plugin generated — a report, an export, an
// attachment — and stores it.
//
// This is the one place binary content moves through the plugin transport.
// Reads never do: a plugin asks for a download token and the browser fetches
// from Core directly, so a large download costs Core a redirect rather than a
// buffered copy.
func (s *Server) PutFile(stream pb.HostServices_PutFileServer) error {
	if err := s.require(PermFilesWrite); err != nil {
		return err
	}
	if s.deps.Files == nil {
		return s.unavailable("file storage")
	}

	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "no file content received")
	}

	// Only the first chunk carries metadata; the rest is payload.
	filename := first.GetFilename()
	mimeType := first.GetMimeType()

	var buf bytes.Buffer
	buf.Write(first.GetData())
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return status.Errorf(codes.Aborted, "receive file: %v", err)
		}
		buf.Write(chunk.GetData())
	}

	fileID, size, err := s.deps.Files.Put(stream.Context(), s.key, filename, mimeType, &buf)
	if err != nil {
		return status.Errorf(codes.Internal, "store file: %v", err)
	}
	return stream.SendAndClose(&pb.PutFileResponse{FileId: fileID, Size: size})
}

func (s *Server) DeleteFile(ctx context.Context, req *pb.FileRequest) (*emptypb.Empty, error) {
	if err := s.require(PermFilesWrite); err != nil {
		return nil, err
	}
	if s.deps.Files == nil {
		return nil, s.unavailable("file storage")
	}
	if err := s.deps.Files.Delete(ctx, s.key, req.GetFileId()); err != nil {
		// A file belonging to another plugin reports as not-found rather than
		// forbidden: confirming that an id exists is itself information.
		return nil, status.Errorf(codes.NotFound, "%v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) GenerateDownloadToken(ctx context.Context, req *pb.DownloadTokenRequest) (*pb.DownloadTokenResponse, error) {
	if err := s.require(PermFilesRead); err != nil {
		return nil, err
	}
	if s.deps.Files == nil {
		return nil, s.unavailable("file storage")
	}

	url, expiresAt, err := s.deps.Files.GenerateDownloadToken(ctx, s.key,
		req.GetFileId(), req.GetUserId(),
		time.Duration(req.GetExpirySeconds())*time.Second)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "issue download token: %v", err)
	}
	return &pb.DownloadTokenResponse{Url: url, ExpiresAtUnix: expiresAt.Unix()}, nil
}

func (s *Server) GetFileMetadata(ctx context.Context, req *pb.FileRequest) (*pb.FileMetadata, error) {
	if err := s.require(PermFilesRead); err != nil {
		return nil, err
	}
	if s.deps.Files == nil {
		return nil, s.unavailable("file storage")
	}

	meta, err := s.deps.Files.Metadata(ctx, s.key, req.GetFileId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &pb.FileMetadata{
		Found:    meta.Found,
		FileId:   meta.FileID,
		Filename: meta.Filename,
		Size:     meta.Size,
		MimeType: meta.MimeType,
	}, nil
}
