package com.ty.sdk;

/** Connection settings for a Java extension. */
public class Config {
    private final String extensionKey;
    private final String coreGrpcUrl;
    private final boolean dev;
    private final String version;
    private final String frontendDir;
    private final String manifestPath;
    private final String extensionSecret;

    public Config(String extensionKey, String coreGrpcUrl, boolean dev) {
        this(extensionKey, coreGrpcUrl, dev, "1.0.0", "", "", "");
    }

    public Config(String extensionKey, String coreGrpcUrl, boolean dev, String version) {
        this(extensionKey, coreGrpcUrl, dev, version, "", "", "");
    }

    public Config(String extensionKey, String coreGrpcUrl, boolean dev, String version, String frontendDir) {
        this(extensionKey, coreGrpcUrl, dev, version, frontendDir, "", "");
    }

    public Config(String extensionKey, String coreGrpcUrl, boolean dev, String version,
                  String frontendDir, String manifestPath) {
        this(extensionKey, coreGrpcUrl, dev, version, frontendDir, manifestPath, "");
    }

    /**
     * @param frontendDir     built micro-frontend directory streamed to Core after
     *                        approval when {@code dev} is false; ignored in dev mode.
     * @param manifestPath    manifest.yaml path; when set the SDK sends the declared
     *                        collections/indexes/slots so Core provisions CMDS tables,
     *                        and persists the issued secret back into it on approval.
     * @param extensionSecret credential authenticating an already-approved extension;
     *                        usually loaded from manifest.yaml. Empty on a first-time
     *                        registration, which Core parks as pending for approval.
     */
    public Config(String extensionKey, String coreGrpcUrl, boolean dev, String version,
                  String frontendDir, String manifestPath, String extensionSecret) {
        this.extensionKey = extensionKey;
        this.coreGrpcUrl = coreGrpcUrl;
        this.dev = dev;
        this.version = version;
        this.frontendDir = frontendDir;
        this.manifestPath = manifestPath;
        this.extensionSecret = extensionSecret;
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

    public String getManifestPath() {
        return manifestPath;
    }

    public String getExtensionSecret() {
        return extensionSecret;
    }
}
