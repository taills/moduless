package tunnel

import (
	"context"
	"errors"

	"github.com/taills/moduless/core/db"
	pb "github.com/taills/moduless/proto/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// extKeyMetadataField is the gRPC metadata key carrying the calling
// extension's identity. The SDK sets it on every DB/File/Event call.
const extKeyMetadataField = "x-extension-key"

type ctxKey string

const extensionKeyCtx ctxKey = "extension_key"

// ExtensionKeyUnaryInterceptor extracts the extension key from incoming gRPC
// metadata and places it in the context so service handlers can enforce
// per-extension data isolation.
func ExtensionKeyUnaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(extKeyMetadataField); len(vals) > 0 && vals[0] != "" {
			ctx = context.WithValue(ctx, extensionKeyCtx, vals[0])
		}
	}
	return handler(ctx, req)
}

// KeyAuthorizer reports whether an extension key is approved. Implemented by the
// extension registry; used to gate data-plane (DB/File/Event) unary calls.
type KeyAuthorizer interface {
	IsApproved(ctx context.Context, key string) bool
}

// ApprovedKeyUnaryInterceptor builds an interceptor that, in addition to placing
// the extension key in context, rejects data-plane calls from keys that are not
// approved. When authz is nil the gate is skipped (open/demo mode).
func ApprovedKeyUnaryInterceptor(authz KeyAuthorizer) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		key := ""
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get(extKeyMetadataField); len(vals) > 0 {
				key = vals[0]
			}
		}
		if key != "" {
			ctx = context.WithValue(ctx, extensionKeyCtx, key)
		}
		if authz != nil {
			if key == "" || !authz.IsApproved(ctx, key) {
				return nil, status.Error(codes.PermissionDenied, "extension not approved")
			}
		}
		return handler(ctx, req)
	}
}

func extensionKeyFromCtx(ctx context.Context) (string, error) {
	if v, ok := ctx.Value(extensionKeyCtx).(string); ok && v != "" {
		return v, nil
	}
	return "", errors.New("missing extension identity in request context")
}

// DbServer implements the DatabaseService gRPC API backed by the CMDS manager.
type DbServer struct {
	pb.UnimplementedDatabaseServiceServer
	CMDS *db.CMDSManager
}

func NewDbServer(cmds *db.CMDSManager) *DbServer {
	return &DbServer{CMDS: cmds}
}

func (s *DbServer) Put(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
	extKey, err := extensionKeyFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.CMDS.Put(extKey, req.Collection, req.DocumentId, req.JsonData); err != nil {
		return &pb.PutResponse{Success: false}, err
	}
	return &pb.PutResponse{Success: true}, nil
}

func (s *DbServer) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	extKey, err := extensionKeyFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	data, found, err := s.CMDS.Get(extKey, req.Collection, req.DocumentId)
	if err != nil {
		return nil, err
	}
	return &pb.GetResponse{Found: found, JsonData: data}, nil
}

func (s *DbServer) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	extKey, err := extensionKeyFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.CMDS.Delete(extKey, req.Collection, req.DocumentId); err != nil {
		return &pb.DeleteResponse{Success: false}, err
	}
	return &pb.DeleteResponse{Success: true}, nil
}

func (s *DbServer) Find(ctx context.Context, req *pb.FindRequest) (*pb.FindResponse, error) {
	extKey, err := extensionKeyFromCtx(ctx)
	if err != nil {
		return nil, err
	}
	filters := make([]db.Filter, 0, len(req.Filters))
	for _, f := range req.Filters {
		filters = append(filters, db.Filter{
			Field:    f.Field,
			Operator: f.Operator,
			Value:    f.Value,
		})
	}
	docs, err := s.CMDS.Find(extKey, req.Collection, filters, int(req.Limit), int(req.Offset))
	if err != nil {
		return nil, err
	}
	return &pb.FindResponse{Documents: docs}, nil
}
