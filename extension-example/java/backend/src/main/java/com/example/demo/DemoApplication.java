package com.example.demo;

import com.ty.sdk.Config;
import com.ty.sdk.bridge.ServletBridge;
import com.ty.sdk.context.UserContext;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.context.ApplicationContext;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.RestController;

import java.util.HashMap;
import java.util.Map;

/**
 * Java extension example. The embedded Tomcat port is disabled; the SDK dials
 * Core's gRPC tunnel instead (set CORE_URL, default localhost:9000).
 *
 * Run: mvn spring-boot:run -pl extension-example/java/backend
 */
@SpringBootApplication
@RestController
public class DemoApplication {

    @GetMapping("/info")
    public Map<String, Object> info() {
        Map<String, Object> body = new HashMap<>();
        UserContext user = UserContext.get();
        body.put("language", "java");
        body.put("user_id", user != null ? user.getUserId() : "anonymous");
        body.put("roles", user != null ? user.getRoles() : java.util.Collections.emptyList());
        return body;
    }

    public static void main(String[] args) {
        ApplicationContext ctx = SpringApplication.run(DemoApplication.class, args);

        String coreUrl = System.getenv().getOrDefault("CORE_URL", "localhost:9000");
        ServletBridge.start(ctx, new Config("java_example", coreUrl, true));
    }
}
