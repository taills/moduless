package sdk

import (
	"context"
	"encoding/json"

	pb "github.com/ty-lab/go-web-module/proto/tunnel"
	"google.golang.org/grpc"
)

// Filter expresses a single CMDS query predicate.
type Filter struct {
	Field    string
	Operator string // "=", ">", "<", "LIKE"
	Value    string
}

// DBClient is the type-safe Core-Managed Document Store client.
type DBClient struct {
	client pb.DatabaseServiceClient
}

func NewDBClient(conn *grpc.ClientConn) *DBClient {
	return &DBClient{client: pb.NewDatabaseServiceClient(conn)}
}

// Put stores a document, marshalling value as JSON.
func (db *DBClient) Put(ctx context.Context, collection, docID string, value interface{}) error {
	jsonData, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = db.client.Put(ctx, &pb.PutRequest{
		Collection: collection,
		DocumentId: docID,
		JsonData:   jsonData,
	})
	return err
}

// Get loads a document into dest. Returns (false, nil) when not found.
func (db *DBClient) Get(ctx context.Context, collection, docID string, dest interface{}) (bool, error) {
	resp, err := db.client.Get(ctx, &pb.GetRequest{
		Collection: collection,
		DocumentId: docID,
	})
	if err != nil {
		return false, err
	}
	if !resp.Found {
		return false, nil
	}
	if err := json.Unmarshal(resp.JsonData, dest); err != nil {
		return false, err
	}
	return true, nil
}

// Delete removes a document by id.
func (db *DBClient) Delete(ctx context.Context, collection, docID string) error {
	_, err := db.client.Delete(ctx, &pb.DeleteRequest{
		Collection: collection,
		DocumentId: docID,
	})
	return err
}

// Find returns the raw JSON documents matching all filters.
func (db *DBClient) Find(ctx context.Context, collection string, filters []Filter, limit, offset int32) ([][]byte, error) {
	pbFilters := make([]*pb.QueryFilter, 0, len(filters))
	for _, f := range filters {
		pbFilters = append(pbFilters, &pb.QueryFilter{
			Field:    f.Field,
			Operator: f.Operator,
			Value:    f.Value,
		})
	}
	resp, err := db.client.Find(ctx, &pb.FindRequest{
		Collection: collection,
		Filters:    pbFilters,
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		return nil, err
	}
	return resp.Documents, nil
}

// FindInto unmarshals all matching documents into dest (a pointer to a slice).
func (db *DBClient) FindInto(ctx context.Context, collection string, filters []Filter, limit, offset int32, dest interface{}) error {
	docs, err := db.Find(ctx, collection, filters, limit, offset)
	if err != nil {
		return err
	}
	// Build a JSON array then unmarshal once into dest.
	buf := make([]byte, 0, 64)
	buf = append(buf, '[')
	for i, d := range docs {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, d...)
	}
	buf = append(buf, ']')
	return json.Unmarshal(buf, dest)
}
