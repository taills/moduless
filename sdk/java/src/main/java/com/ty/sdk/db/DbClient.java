package com.ty.sdk.db;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.google.protobuf.ByteString;
import com.ty.sdk.proto.DatabaseServiceGrpc;
import com.ty.sdk.proto.GetRequest;
import com.ty.sdk.proto.GetResponse;
import com.ty.sdk.proto.PutRequest;
import io.grpc.Channel;
import io.grpc.Metadata;
import io.grpc.stub.MetadataUtils;

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
}
