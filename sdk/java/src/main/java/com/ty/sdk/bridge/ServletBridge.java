package com.ty.sdk.bridge;

import com.google.protobuf.ByteString;
import com.ty.sdk.Config;
import com.ty.sdk.context.UserContext;
import com.ty.sdk.proto.ExtensionTunnelGrpc;
import com.ty.sdk.proto.HttpRequestChunk;
import com.ty.sdk.proto.HttpResponseChunk;
import com.ty.sdk.proto.RegisterRequest;
import com.ty.sdk.proto.RegisterResponse;
import com.ty.sdk.proto.TunnelMessage;
import io.grpc.ManagedChannel;
import io.grpc.ManagedChannelBuilder;
import io.grpc.stub.StreamObserver;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import org.springframework.context.ApplicationContext;
import org.springframework.mock.web.MockHttpServletRequest;
import org.springframework.web.servlet.DispatcherServlet;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Map;

/**
 * Bridges Core's reverse gRPC tunnel into Spring's DispatcherServlet. The
 * extension opens no listening port; it dials Core and serves requests that
 * arrive over the bidirectional stream.
 */
public class ServletBridge {

    public static void start(ApplicationContext ctx, Config config) {
        DispatcherServlet dispatcher = ctx.getBean(DispatcherServlet.class);
        ManagedChannel channel = ManagedChannelBuilder
                .forTarget(config.getCoreGrpcUrl())
                .usePlaintext()
                .build();
        ExtensionTunnelGrpc.ExtensionTunnelStub stub = ExtensionTunnelGrpc.newStub(channel);

        final StreamObserver<TunnelMessage>[] requestObserverHolder = new StreamObserver[1];

        StreamObserver<TunnelMessage> responseObserver = new StreamObserver<>() {
            @Override
            public void onNext(TunnelMessage msg) {
                if (msg.hasRegisterResp()) {
                    RegisterResponse resp = msg.getRegisterResp();
                    if (!resp.getSuccess()) {
                        System.err.println("registration rejected: " + resp.getErrorMessage());
                    } else {
                        System.out.println("registration success");
                    }
                } else if (msg.hasHttpReqChunk()) {
                    handleRequest(dispatcher, msg.getHttpReqChunk(), requestObserverHolder[0]);
                }
            }

            @Override
            public void onError(Throwable t) {
                System.err.println("tunnel error: " + t.getMessage());
            }

            @Override
            public void onCompleted() {
                System.out.println("tunnel closed by core");
            }
        };

        StreamObserver<TunnelMessage> requestObserver = stub.connect(responseObserver);
        requestObserverHolder[0] = requestObserver;

        requestObserver.onNext(TunnelMessage.newBuilder()
                .setRegisterReq(RegisterRequest.newBuilder()
                        .setExtensionKey(config.getExtensionKey())
                        .setVersion(config.getVersion())
                        .setIsDev(config.isDev())
                        .build())
                .build());
    }

    private static void handleRequest(DispatcherServlet dispatcher, HttpRequestChunk chunk,
                                      StreamObserver<TunnelMessage> out) {
        try {
            // Spring's MockHttpServletRequest provides a complete servlet base
            // that DispatcherServlet can route; we layer tunnel data on top.
            MockHttpServletRequest base = new MockHttpServletRequest();
            base.setContextPath("");

            com.ty.sdk.bridge.MockHttpServletRequest req =
                    new com.ty.sdk.bridge.MockHttpServletRequest(
                            base, chunk.getMethod(), chunk.getPath(), chunk.getQuery(),
                            chunk.getHeadersMap(), chunk.getBodyChunk().toByteArray());

            org.springframework.mock.web.MockHttpServletResponse baseResp =
                    new org.springframework.mock.web.MockHttpServletResponse();
            MockHttpServletResponse resp = new MockHttpServletResponse(baseResp);

            injectUser(chunk.getHeadersMap());
            try {
                dispatcher.service((HttpServletRequest) req, (HttpServletResponse) resp);
            } finally {
                UserContext.clear();
            }

            HttpResponseChunk.Builder respChunk = HttpResponseChunk.newBuilder()
                    .setStreamId(chunk.getStreamId())
                    .setIsFirst(true)
                    .setIsLast(true)
                    .setStatusCode(resp.getCapturedStatus())
                    .setBodyChunk(ByteString.copyFrom(resp.getCapturedBody()));
            for (Map.Entry<String, String> e : resp.getCapturedHeaders().entrySet()) {
                respChunk.putHeaders(e.getKey(), e.getValue());
            }

            synchronized (out) {
                out.onNext(TunnelMessage.newBuilder().setHttpRespChunk(respChunk.build()).build());
            }
        } catch (Exception e) {
            HttpResponseChunk err = HttpResponseChunk.newBuilder()
                    .setStreamId(chunk.getStreamId())
                    .setIsFirst(true)
                    .setIsLast(true)
                    .setStatusCode(500)
                    .putHeaders("Content-Type", "text/plain")
                    .setBodyChunk(ByteString.copyFromUtf8("internal error: " + e.getMessage()))
                    .build();
            synchronized (out) {
                out.onNext(TunnelMessage.newBuilder().setHttpRespChunk(err).build());
            }
        }
    }

    private static void injectUser(Map<String, String> headers) {
        String userId = headers.get("X-User-Id");
        if (userId == null || userId.isEmpty()) {
            return;
        }
        UserContext.set(new UserContext(userId,
                splitCsv(headers.get("X-User-Roles")),
                splitCsv(headers.get("X-User-Permissions"))));
    }

    private static List<String> splitCsv(String v) {
        if (v == null || v.isEmpty()) {
            return new ArrayList<>();
        }
        return new ArrayList<>(Arrays.asList(v.split(",")));
    }
}
