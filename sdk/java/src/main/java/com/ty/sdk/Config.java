package com.ty.sdk;

/** Connection settings for a Java extension. */
public class Config {
    private final String extensionKey;
    private final String coreGrpcUrl;
    private final boolean dev;
    private final String version;
    private final String frontendDir;

    public Config(String extensionKey, String coreGrpcUrl, boolean dev) {
        this(extensionKey, coreGrpcUrl, dev, "1.0.0", "");
    }

    public Config(String extensionKey, String coreGrpcUrl, boolean dev, String version) {
        this(extensionKey, coreGrpcUrl, dev, version, "");
    }

    /**
     * @param frontendDir built micro-frontend directory streamed to Core during
     *                    registration when {@code dev} is false; ignored in dev mode.
     */
    public Config(String extensionKey, String coreGrpcUrl, boolean dev, String version, String frontendDir) {
        this.extensionKey = extensionKey;
        this.coreGrpcUrl = coreGrpcUrl;
        this.dev = dev;
        this.version = version;
        this.frontendDir = frontendDir;
    }

    public String getExtensionKey() {
        return extensionKey;
    }

    public String getCoreGrpcUrl() {
        return coreGrpcUrl;
    }

    public boolean isDev() {
        return dev;
    }

    public String getVersion() {
        return version;
    }

    public String getFrontendDir() {
        return frontendDir;
    }
}
