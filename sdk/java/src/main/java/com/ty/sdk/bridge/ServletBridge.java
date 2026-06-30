package com.ty.sdk.bridge;

import com.google.protobuf.ByteString;
import com.ty.sdk.Config;
import com.ty.sdk.context.UserContext;
import com.ty.sdk.manifest.ManifestLoader;
import com.ty.sdk.proto.ExtensionTunnelGrpc;
import com.ty.sdk.proto.FileChunk;
import com.ty.sdk.proto.HttpRequestChunk;
import com.ty.sdk.proto.HttpResponseChunk;
import com.ty.sdk.proto.RegisterComplete;
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
import org.springframework.mock.web.MockServletConfig;
import org.springframework.mock.web.MockServletContext;
import org.springframework.web.context.WebApplicationContext;
import org.springframework.web.servlet.DispatcherServlet;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Map;
import java.util.stream.Stream;

/**
 * Bridges Core's reverse gRPC tunnel into Spring's DispatcherServlet. The
 * extension opens no listening port; it dials Core and serves requests that
 * arrive over the bidirectional stream.
 */
public class ServletBridge {

    /** Keep each FileChunk under gRPC's 4MB default message limit. */
    private static final int FRONTEND_CHUNK_SIZE = 256 * 1024;

    public static void start(ApplicationContext ctx, Config config) {
        // Build a DispatcherServlet bound to the Spring context and initialise it
        // with a mock servlet config. Without init() the servlet has no handler
        // mappings and getServletConfig() is null, so dispatching every request
        // would 404. (server.port=-1 means no real container does this for us.)
        DispatcherServlet dispatcher = new DispatcherServlet((WebApplicationContext) ctx);
        try {
            dispatcher.init(new MockServletConfig(new MockServletContext(), "moduless"));
        } catch (Exception e) {
            throw new IllegalStateException("failed to initialise DispatcherServlet: " + e.getMessage(), e);
        }

        ManagedChannel channel = ManagedChannelBuilder
                .forTarget(config.getCoreGrpcUrl())
                .usePlaintext()
                .build();
        ExtensionTunnelGrpc.ExtensionTunnelStub stub = ExtensionTunnelGrpc.newStub(channel);

        // Bundle the micro-frontend once so it can be streamed after register.
        byte[] frontendZip = new byte[0];
        boolean registerAsDev = config.isDev();
        String zipSha256 = "";
        if (!config.isDev()) {
            if (config.getFrontendDir() != null && !config.getFrontendDir().isEmpty()) {
                try {
                    frontendZip = buildFrontendZip(config.getFrontendDir());
                    zipSha256 = sha256Hex(frontendZip);
                } catch (Exception e) {
                    throw new IllegalStateException("failed to bundle frontend: " + e.getMessage(), e);
                }
            } else {
                System.out.println("no frontendDir set with dev=false; registering without a micro-frontend");
                registerAsDev = true;
            }
        }
        final byte[] zipBytes = frontendZip;

        // Resolve the approval secret: an explicit config value wins, else the one
        // persisted in manifest.yaml after a previous approval.
        String resolvedSecret = config.getExtensionSecret();
        if ((resolvedSecret == null || resolvedSecret.isEmpty())
                && config.getManifestPath() != null && !config.getManifestPath().isEmpty()) {
            try {
                resolvedSecret = ManifestLoader.loadSecret(config.getManifestPath());
            } catch (Exception e) {
                System.err.println("warning: failed to read manifest secret: " + e.getMessage());
            }
        }
        final String secret = resolvedSecret == null ? "" : resolvedSecret;

        final StreamObserver<TunnelMessage>[] requestObserverHolder = new StreamObserver[1];

        StreamObserver<TunnelMessage> responseObserver = new StreamObserver<>() {
            @Override
            public void onNext(TunnelMessage msg) {
                if (msg.hasRegisterDecision()) {
                    handleDecision(msg.getRegisterDecision(), requestObserverHolder[0], zipBytes, config);
                } else if (msg.hasRegisterResp()) {
                    RegisterResponse resp = msg.getRegisterResp();
                    if (!resp.getSuccess()) {
                        System.err.println("registration failed: " + resp.getErrorMessage());
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

        RegisterRequest.Builder reg = RegisterRequest.newBuilder()
                .setExtensionKey(config.getExtensionKey())
                .setVersion(config.getVersion())
                .setIsDev(registerAsDev)
                .setZipFileSize(zipBytes.length)
                .setZipSha256(zipSha256)
                .setExtensionSecret(secret);
        // Send declared collections/indexes/slots so Core provisions CMDS tables.
        if (config.getManifestPath() != null && !config.getManifestPath().isEmpty()) {
            try {
                ManifestLoader.apply(config.getManifestPath(), reg);
            } catch (Exception e) {
                System.err.println("warning: failed to load manifest " + config.getManifestPath() + ": " + e.getMessage());
            }
        }

        // Register; the frontend is uploaded only after Core approves (decision).
        requestObserver.onNext(TunnelMessage.newBuilder().setRegisterReq(reg.build()).build());
    }

    /**
     * Handles a RegisterDecision: a pending decision is awaited, an approval
     * persists the issued secret and (in production) uploads the frontend, and a
     * rejection is logged.
     */
    private static void handleDecision(com.ty.sdk.proto.RegisterDecision d,
                                       StreamObserver<TunnelMessage> out, byte[] zipBytes, Config config) {
        switch (d.getStatus()) {
            case "pending" -> System.out.println("registration pending administrator approval...");
            case "approved" -> {
                if (!d.getIssuedSecret().isEmpty()) {
                    if (config.getManifestPath() != null && !config.getManifestPath().isEmpty()) {
                        try {
                            ManifestLoader.saveSecret(config.getManifestPath(), d.getIssuedSecret());
                            System.out.println("approved; persisted issued secret to " + config.getManifestPath());
                        } catch (Exception e) {
                            System.err.println("warning: failed to persist secret: " + e.getMessage());
                        }
                    } else {
                        System.out.println("approved; no manifestPath set, secret not persisted");
                    }
                }
                if (d.getUploadFrontend() && zipBytes.length > 0) {
                    uploadFrontend(out, zipBytes);
                }
            }
            case "rejected" -> System.err.println("registration rejected by administrator");
            default -> { }
        }
    }

    /** Streams the bundled frontend then signals completion. */
    private static void uploadFrontend(StreamObserver<TunnelMessage> out, byte[] zipBytes) {
        int index = 0;
        for (int offset = 0; offset < zipBytes.length; offset += FRONTEND_CHUNK_SIZE) {
            int end = Math.min(offset + FRONTEND_CHUNK_SIZE, zipBytes.length);
            synchronized (out) {
                out.onNext(TunnelMessage.newBuilder()
                        .setFileChunk(FileChunk.newBuilder()
                                .setContent(ByteString.copyFrom(zipBytes, offset, end - offset))
                                .setChunkIndex(index++)
                                .build())
                        .build());
            }
        }
        synchronized (out) {
            out.onNext(TunnelMessage.newBuilder()
                    .setRegisterComplete(RegisterComplete.newBuilder().build())
                    .build());
        }
    }

    /** Zip the directory with slash-separated relative entry names. */
    private static byte[] buildFrontendZip(String dir) throws IOException {
        Path root = Path.of(dir);
        if (!Files.isDirectory(root)) {
            throw new IOException("frontend dir not found: " + dir);
        }
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        try (java.util.zip.ZipOutputStream zos = new java.util.zip.ZipOutputStream(out);
             Stream<Path> paths = Files.walk(root)) {
            List<Path> files = paths.filter(Files::isRegularFile).toList();
            for (Path file : files) {
                String entry = root.relativize(file).toString().replace('\\', '/');
                zos.putNextEntry(new java.util.zip.ZipEntry(entry));
                Files.copy(file, zos);
                zos.closeEntry();
            }
        }
        return out.toByteArray();
    }

    private static String sha256Hex(byte[] data) {
        try {
            byte[] digest = MessageDigest.getInstance("SHA-256").digest(data);
            StringBuilder sb = new StringBuilder(digest.length * 2);
            for (byte b : digest) {
                sb.append(Character.forDigit((b >> 4) & 0xF, 16));
                sb.append(Character.forDigit(b & 0xF, 16));
            }
            return sb.toString();
        } catch (Exception e) {
            throw new IllegalStateException("SHA-256 unavailable", e);
        }
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
