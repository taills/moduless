package com.example.demo;

import com.ty.sdk.Config;
import com.ty.sdk.bridge.ServletBridge;
import com.ty.sdk.db.DbClient;
import io.grpc.ManagedChannelBuilder;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.context.ApplicationContext;
import org.springframework.context.annotation.Bean;

/**
 * Java extension example. The embedded Tomcat port is disabled; the SDK dials
 * Core's gRPC tunnel instead.
 *
 * Dev:  mvn spring-boot:run -pl extension-example/java/backend
 * Prod: set FRONTEND_DIR so the SDK ships the built dist to Core.
 */
@SpringBootApplication
public class DemoApplication {

    static final String EXTENSION_KEY = "java_example";

    private static String coreUrl() {
        return System.getenv().getOrDefault("CORE_URL", "localhost:9000");
    }

    /** CMDS client over a dedicated channel; carries x-extension-key metadata. */
    @Bean
    public DbClient dbClient() {
        return new DbClient(
                ManagedChannelBuilder.forTarget(coreUrl()).usePlaintext().build(),
                EXTENSION_KEY);
    }

    public static void main(String[] args) {
        ApplicationContext ctx = SpringApplication.run(DemoApplication.class, args);

        String frontendDir = System.getenv().getOrDefault("FRONTEND_DIR", "");
        String manifestPath = System.getenv().getOrDefault("MANIFEST_PATH", "../manifest.yaml");
        boolean dev = frontendDir.isEmpty();
        ServletBridge.start(ctx, new Config(EXTENSION_KEY, coreUrl(), dev, "1.0.0", frontendDir, manifestPath));
    }
}
