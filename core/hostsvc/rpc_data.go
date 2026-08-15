package hostsvc

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/taills/moduless/core/db"
	pb "github.com/taills/moduless/proto/plugin"
)

// dataErr maps store failures onto gRPC codes a plugin can act on.
//
// The distinction matters: a version conflict means "re-read and retry", a
// dead transaction means "start over", and everything else is an internal
// fault the plugin can do nothing about. Collapsing them all into Internal
// would leave plugin authors guessing.
func dataErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, db.ErrVersionConflict):
		return status.Error(codes.FailedPrecondition,
			"version conflict: the document changed since you read it")
	case errors.Is(err, db.ErrUnknownTx):
		return status.Error(codes.FailedPrecondition,
			"unknown or expired transaction; transactions are rolled back once their timeout passes")
	default:
		return status.Errorf(codes.Internal, "%v", err)
	}
}

func (s *Server) dataBackend() (DataBackend, error) {
	if s.deps.Data == nil {
		return nil, s.unavailable("the document store")
	}
	return s.deps.Data, nil
}

// requireTxPermission gates transactional access separately from plain reads
// and writes: an open transaction holds a database connection, so it is a
// heavier grant than a single statement.
func (s *Server) requireTx(txID string) error {
	if txID == "" {
		return nil
	}
	return s.require(PermDBTx)
}

func (s *Server) Put(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
	if err := s.require(PermDB); err != nil {
		return nil, err
	}
	if err := s.requireTx(req.GetTxId()); err != nil {
		return nil, err
	}
	data, err := s.dataBackend()
	if err != nil {
		return nil, err
	}

	version, err := data.Put(ctx, s.key, req.GetCollection(), req.GetDocId(),
		req.GetData(), req.GetTxId(), req.GetExpectedVersion())
	if err != nil {
		return nil, dataErr(err)
	}
	return &pb.PutResponse{Success: true, Version: version}, nil
}

func (s *Server) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	if err := s.require(PermDB); err != nil {
		return nil, err
	}
	if err := s.requireTx(req.GetTxId()); err != nil {
		return nil, err
	}
	data, err := s.dataBackend()
	if err != nil {
		return nil, err
	}

	doc, version, found, err := data.Get(ctx, s.key, req.GetCollection(), req.GetDocId(), req.GetTxId())
	if err != nil {
		return nil, dataErr(err)
	}
	return &pb.GetResponse{Found: found, Data: doc, Version: version}, nil
}

func (s *Server) Delete(ctx context.Context, req *pb.DeleteRequest) (*pb.DeleteResponse, error) {
	if err := s.require(PermDB); err != nil {
		return nil, err
	}
	if err := s.requireTx(req.GetTxId()); err != nil {
		return nil, err
	}
	data, err := s.dataBackend()
	if err != nil {
		return nil, err
	}

	ok, err := data.Delete(ctx, s.key, req.GetCollection(), req.GetDocId(), req.GetTxId())
	if err != nil {
		return nil, dataErr(err)
	}
	return &pb.DeleteResponse{Success: ok}, nil
}

func (s *Server) Find(ctx context.Context, req *pb.FindRequest) (*pb.FindResponse, error) {
	if err := s.require(PermDB); err != nil {
		return nil, err
	}
	if err := s.requireTx(req.GetTxId()); err != nil {
		return nil, err
	}
	data, err := s.dataBackend()
	if err != nil {
		return nil, err
	}

	docs, err := data.Find(ctx, s.key, req.GetCollection(), filtersFrom(req.GetFilters()),
		int(req.GetLimit()), int(req.GetOffset()), req.GetTxId())
	if err != nil {
		return nil, dataErr(err)
	}
	return &pb.FindResponse{Documents: docs}, nil
}

func (s *Server) Query(ctx context.Context, req *pb.QueryRequest) (*pb.QueryResponse, error) {
	if err := s.require(PermDB); err != nil {
		return nil, err
	}
	if err := s.requireTx(req.GetTxId()); err != nil {
		return nil, err
	}
	data, err := s.dataBackend()
	if err != nil {
		return nil, err
	}

	sorts := make([]Sort, 0, len(req.GetSort()))
	for _, srt := range req.GetSort() {
		sorts = append(sorts, Sort{Field: srt.GetField(), Descending: srt.GetDescending()})
	}

	res, err := data.Query(ctx, s.key, req.GetCollection(), QueryOptions{
		Filters: filtersFrom(req.GetFilters()),
		Sort:    sorts,
		Limit:   int(req.GetLimit()),
		Cursor:  req.GetCursor(),
	}, req.GetTxId())
	if err != nil {
		// A malformed cursor or an unsupported sort is the caller's mistake,
		// not an internal fault.
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	return &pb.QueryResponse{
		Documents:  res.Documents,
		NextCursor: res.NextCursor,
		HasMore:    res.HasMore,
	}, nil
}

func (s *Server) Aggregate(ctx context.Context, req *pb.AggregateRequest) (*pb.AggregateResponse, error) {
	if err := s.require(PermDB); err != nil {
		return nil, err
	}
	if err := s.requireTx(req.GetTxId()); err != nil {
		return nil, err
	}
	data, err := s.dataBackend()
	if err != nil {
		return nil, err
	}

	buckets, err := data.Aggregate(ctx, s.key, req.GetCollection(), AggregateOptions{
		Filters: filtersFrom(req.GetFilters()),
		Func:    aggregateFuncName(req.GetFunc()),
		Field:   req.GetField(),
		GroupBy: req.GetGroupBy(),
	}, req.GetTxId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	out := make([]*pb.AggregateResponse_Bucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, &pb.AggregateResponse_Bucket{Keys: b.Keys, Value: b.Value})
	}
	return &pb.AggregateResponse{Buckets: out}, nil
}

func (s *Server) BatchWrite(ctx context.Context, req *pb.BatchWriteRequest) (*pb.BatchWriteResponse, error) {
	if err := s.require(PermDB); err != nil {
		return nil, err
	}
	if err := s.requireTx(req.GetTxId()); err != nil {
		return nil, err
	}
	data, err := s.dataBackend()
	if err != nil {
		return nil, err
	}

	ops := make([]WriteOp, 0, len(req.GetOps()))
	for _, op := range req.GetOps() {
		switch kind := op.GetKind().(type) {
		case *pb.BatchWriteRequest_Op_Put:
			ops = append(ops, WriteOp{
				Collection: kind.Put.GetCollection(),
				DocID:      kind.Put.GetDocId(),
				Data:       kind.Put.GetData(),
			})
		case *pb.BatchWriteRequest_Op_Delete:
			ops = append(ops, WriteOp{
				Delete:     true,
				Collection: kind.Delete.GetCollection(),
				DocID:      kind.Delete.GetDocId(),
			})
		default:
			return nil, status.Error(codes.InvalidArgument, "batch op has no put or delete")
		}
	}

	applied, err := data.BatchWrite(ctx, s.key, ops, req.GetTxId())
	if err != nil {
		return nil, dataErr(err)
	}
	return &pb.BatchWriteResponse{Applied: int32(applied)}, nil
}

func (s *Server) BeginTx(ctx context.Context, req *pb.BeginTxRequest) (*pb.BeginTxResponse, error) {
	if err := s.require(PermDBTx); err != nil {
		return nil, err
	}
	data, err := s.dataBackend()
	if err != nil {
		return nil, err
	}

	txID, err := data.BeginTx(ctx, s.key, time.Duration(req.GetTimeoutSeconds())*time.Second)
	if err != nil {
		return nil, dataErr(err)
	}
	return &pb.BeginTxResponse{TxId: txID}, nil
}

func (s *Server) CommitTx(ctx context.Context, req *pb.TxRequest) (*emptypb.Empty, error) {
	if err := s.require(PermDBTx); err != nil {
		return nil, err
	}
	data, err := s.dataBackend()
	if err != nil {
		return nil, err
	}
	if err := data.CommitTx(ctx, s.key, req.GetTxId()); err != nil {
		return nil, dataErr(err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) RollbackTx(ctx context.Context, req *pb.TxRequest) (*emptypb.Empty, error) {
	if err := s.require(PermDBTx); err != nil {
		return nil, err
	}
	data, err := s.dataBackend()
	if err != nil {
		return nil, err
	}
	if err := data.RollbackTx(ctx, s.key, req.GetTxId()); err != nil {
		return nil, dataErr(err)
	}
	return &emptypb.Empty{}, nil
}

func filtersFrom(in []*pb.Filter) []Filter {
	out := make([]Filter, 0, len(in))
	for _, f := range in {
		out = append(out, Filter{
			Field:  f.GetField(),
			Op:     operatorName(f.GetOp()),
			Values: f.GetValues(),
		})
	}
	return out
}

func operatorName(op pb.Operator) string {
	switch op {
	case pb.Operator_OP_NE:
		return db.OpNe
	case pb.Operator_OP_GT:
		return db.OpGt
	case pb.Operator_OP_GTE:
		return db.OpGte
	case pb.Operator_OP_LT:
		return db.OpLt
	case pb.Operator_OP_LTE:
		return db.OpLte
	case pb.Operator_OP_LIKE:
		return db.OpLike
	case pb.Operator_OP_IN:
		return db.OpIn
	case pb.Operator_OP_BETWEEN:
		return db.OpBetween
	case pb.Operator_OP_IS_NULL:
		return db.OpIsNull
	case pb.Operator_OP_IS_NOT_NULL:
		return db.OpIsNotNull
	default:
		return db.OpEq
	}
}

func aggregateFuncName(fn pb.AggregateFunc) string {
	switch fn {
	case pb.AggregateFunc_AGG_SUM:
		return db.AggSum
	case pb.AggregateFunc_AGG_AVG:
		return db.AggAvg
	case pb.AggregateFunc_AGG_MIN:
		return db.AggMin
	case pb.AggregateFunc_AGG_MAX:
		return db.AggMax
	default:
		return db.AggCount
	}
}
