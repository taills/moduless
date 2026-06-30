package com.ty.sdk.db;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.google.protobuf.ByteString;
import com.ty.sdk.proto.DatabaseServiceGrpc;
import com.ty.sdk.proto.DeleteRequest;
import com.ty.sdk.proto.FindRequest;
import com.ty.sdk.proto.FindResponse;
import com.ty.sdk.proto.GetRequest;
import com.ty.sdk.proto.GetResponse;
import com.ty.sdk.proto.PutRequest;
import com.ty.sdk.proto.QueryFilter;
import io.grpc.Channel;
import io.grpc.Metadata;
import io.grpc.stub.MetadataUtils;

import java.util.ArrayList;
import java.util.List;

/**
 * Type-safe CMDS client. Every call carries the {@code x-extension-key}
 * metadata so Core enforces per-extension data isolation. Extensions never
 * connect to PostgreSQL directly.
 */
public class DbClient {
    private static final Metadata.Key<String> EXT_KEY =
            Metadata.Key.of("x-extension-key", Metadata.ASCII_STRING_MARSHALLER);

    private final DatabaseServiceGrpc.DatabaseServiceBlockingStub stub;
    private final ObjectMapper mapper = new ObjectMapper();

    public DbClient(Channel channel, String extensionKey) {
        Metadata md = new Metadata();
        md.put(EXT_KEY, extensionKey);
        this.stub = DatabaseServiceGrpc.newBlockingStub(channel)
                .withInterceptors(MetadataUtils.newAttachHeadersInterceptor(md));
    }

    public <T> void put(String collection, String docId, T value) throws Exception {
        byte[] json = mapper.writeValueAsBytes(value);
        stub.put(PutRequest.newBuilder()
                .setCollection(collection)
                .setDocumentId(docId)
                .setJsonData(ByteString.copyFrom(json))
                .build());
    }

    public <T> T get(String collection, String docId, Class<T> clazz) throws Exception {
        GetResponse resp = stub.get(GetRequest.newBuilder()
                .setCollection(collection)
                .setDocumentId(docId)
                .build());
        if (!resp.getFound()) {
            return null;
        }
        return mapper.readValue(resp.getJsonData().toByteArray(), clazz);
    }

    /** Removes a document; a missing document is not an error. */
    public void delete(String collection, String docId) {
        stub.delete(DeleteRequest.newBuilder()
                .setCollection(collection)
                .setDocumentId(docId)
                .build());
    }

    /** Lists documents in a collection, newest provisioning order, with paging. */
    public <T> List<T> find(String collection, int limit, int offset, Class<T> clazz) throws Exception {
        return find(collection, List.of(), limit, offset, clazz);
    }

    /** Lists documents matching a single equality filter (e.g. status = active). */
    public <T> List<T> find(String collection, String field, String operator, String value,
                            int limit, int offset, Class<T> clazz) throws Exception {
        return find(collection, List.of(QueryFilter.newBuilder()
                .setField(field).setOperator(operator).setValue(value).build()), limit, offset, clazz);
    }

    private <T> List<T> find(String collection, List<QueryFilter> filters,
                             int limit, int offset, Class<T> clazz) throws Exception {
        FindResponse resp = stub.find(FindRequest.newBuilder()
                .setCollection(collection)
                .addAllFilters(filters)
                .setLimit(limit)
                .setOffset(offset)
                .build());
        List<T> out = new ArrayList<>(resp.getDocumentsCount());
        for (ByteString doc : resp.getDocumentsList()) {
            out.add(mapper.readValue(doc.toByteArray(), clazz));
        }
        return out;
    }
}
