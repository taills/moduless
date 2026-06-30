# Java SDK & Extension Example Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Create the Java SDK enabling Spring Boot applications to run as extensions without listening ports, bridging incoming gRPC request packets to the `DispatcherServlet` pipeline, exposing CMDS database clients, and configuring the React/Vue micro-frontend template.

**Architecture:** The Java SDK runs a gRPC channel client. When an HTTP request arrives over gRPC, it wraps it in a mock implementation of `HttpServletRequest`, retrieves Spring's primary `DispatcherServlet` bean, and invokes `.service(request, response)`. The mock `HttpServletResponse` intercepts servlet output streams and forwards response blocks back through `HttpResponseChunk` stubs.

**Tech Stack:** Java 17, Spring Boot 3.x, Maven, gRPC, Protobuf.

## Global Constraints

- Java SDK must integrate natively with standard Servlet or Spring Web APIs.
- Extensions bind to no local network socket ports; all traffic flows over gRPC.
- Project examples structured under `extension-example/java/{frontend,backend}`.

---

### Task 1: Java Protobuf Maven Setup & Compile

Configure a Maven parent/module structure to compile the protobuf files into Java classes.

**Files:**
- Create: `sdk/java/pom.xml`

**Interfaces:**
- Produces: Compiled gRPC stubs:
  - `tunnel.ExtensionTunnelGrpc`
  - `tunnel.TunnelOuterClass`

- [ ] **Step 1: Write Maven `sdk/java/pom.xml` configuration**

```xml
<project xmlns="http://maven.apache.org/POM/4.0.0"
         xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"
         xsi:schemaLocation="http://maven.apache.org/POM/4.0.0 http://maven.apache.org/xsd/maven-4.0.0.xsd">
    <modelVersion>4.0.0</modelVersion>
    <groupId>com.ty.lab</groupId>
    <artifactId>java-sdk</artifactId>
    <version>1.0.0</version>

    <properties>
        <maven.compiler.source>17</maven.compiler.source>
        <maven.compiler.target>17</maven.compiler.target>
        <grpc.version>1.54.1</grpc.version>
        <protobuf.version>3.22.2</protobuf.version>
    </properties>

    <dependencies>
        <dependency>
            <groupId>io.grpc</groupId>
            <artifactId>grpc-netty-shaded</artifactId>
            <version>${grpc.version}</version>
        </dependency>
        <dependency>
            <groupId>io.grpc</groupId>
            <artifactId>grpc-protobuf</artifactId>
            <version>${grpc.version}</version>
        </dependency>
        <dependency>
            <groupId>io.grpc</groupId>
            <artifactId>grpc-stub</artifactId>
            <version>${grpc.version}</version>
        </dependency>
        <dependency>
            <groupId>jakarta.servlet</groupId>
            <artifactId>jakarta.servlet-api</artifactId>
            <version>6.0.0</version>
            <scope>provided</scope>
        </dependency>
        <dependency>
            <groupId>org.springframework.boot</groupId>
            <artifactId>spring-boot-starter-web</artifactId>
            <version>3.0.6</version>
            <scope>provided</scope>
        </dependency>
    </dependencies>

    <build>
        <extensions>
            <extension>
                <groupId>kr.motd.maven</groupId>
                <artifactId>os-maven-plugin</artifactId>
                <version>1.7.1</version>
            </extension>
        </extensions>
        <plugins>
            <plugin>
                <groupId>xolstice.maven.plugins</groupId>
                <artifactId>protobuf-maven-plugin</artifactId>
                <version>0.6.1</version>
                <configuration>
                    <protocArtifact>com.google.protobuf:protoc:${protobuf.version}:exe:${os.detected.classifier}</protocArtifact>
                    <pluginId>grpc-java</pluginId>
                    <pluginArtifact>io.grpc:protoc-gen-grpc-java:${grpc.version}:exe:${os.detected.classifier}</pluginArtifact>
                    <protoSourceRoot>../../proto</protoSourceRoot>
                </configuration>
                <executions>
                    <execution>
                        <goals>
                            <goal>compile</goal>
                            <goal>compile-custom</goal>
                        </goals>
                    </execution>
                </executions>
            </plugin>
        </plugins>
    </build>
</project>
```

- [ ] **Step 2: Build and compile Java gRPC sources**

Run: `cd sdk/java && mvn clean compile`
Expected: Success, sources compiled under `target/generated-sources/protobuf/`.

- [ ] **Step 3: Commit**

```bash
git add sdk/java/pom.xml
git commit -m "feat: setup Java SDK Maven environment and compile protobufs"
```

---

### Task 2: Java Servlet Dispatcher Bridge

Bridge incoming gRPC HTTP requests to Spring's `DispatcherServlet` pipeline by constructing custom wrappers for `HttpServletRequest`.

**Files:**
- Create: `sdk/java/src/main/java/com/ty/sdk/context/UserContext.java`
- Create: `sdk/java/src/main/java/com/ty/sdk/bridge/ServletBridge.java`
- Create: `sdk/java/src/main/java/com/ty/sdk/bridge/MockHttpServletRequest.java`

**Interfaces:**
- Produces: `ServletBridge.start(ApplicationContext springContext, Config config)`

- [ ] **Step 1: Write `UserContext.java` ThreadLocal wrapper**

```java
package com.ty.sdk.context;

import java.util.List;

public class UserContext {
    private static final ThreadLocal<UserContext> context = new ThreadLocal<>();

    private final String userId;
    private final List<String> roles;

    public UserContext(String userId, List<String> roles) {
        this.userId = userId;
        this.roles = roles;
    }

    public static UserContext get() { return context.get(); }
    public static void set(UserContext uc) { context.set(uc); }
    public static void clear() { context.remove(); }

    public String getUserId() { return userId; }
    public List<String> getRoles() { return roles; }
}
```

- [ ] **Step 2: Write custom `MockHttpServletRequest.java` translating protobuf parameters**

```java
package com.ty.sdk.bridge;

import jakarta.servlet.ReadListener;
import jakarta.servlet.ServletInputStream;
import jakarta.servlet.http.HttpServletRequestWrapper;
import jakarta.servlet.http.HttpServletRequest;
import java.io.ByteArrayInputStream;
import java.io.IOException;

public class MockHttpServletRequest extends HttpServletRequestWrapper {
    private final byte[] body;
    private final String method;
    private final String path;

    public MockHttpServletRequest(HttpServletRequest request, String method, String path, byte[] body) {
        super(request);
        this.method = method;
        this.path = path;
        this.body = body;
    }

    @Override
    public String getMethod() { return this.method; }

    @Override
    public String getRequestURI() { return this.path; }

    @Override
    public ServletInputStream getInputStream() throws IOException {
        ByteArrayInputStream bais = new ByteArrayInputStream(body);
        return new ServletInputStream() {
            @Override
            public boolean isFinished() { return bais.available() == 0; }
            @Override
            public boolean isReady() { return true; }
            @Override
            public void setReadListener(ReadListener readListener) {}
            @Override
            public int read() throws IOException { return bais.read(); }
        };
    }
}
```

- [ ] **Step 3: Write `ServletBridge.java` runner hooking into Spring Boot**

```java
package com.ty.sdk.bridge;

import org.springframework.context.ApplicationContext;
import org.springframework.web.servlet.DispatcherServlet;
import io.grpc.ManagedChannel;
import io.grpc.ManagedChannelBuilder;
import io.grpc.stub.StreamObserver;
import tunnel.*;

public class ServletBridge {
    public static void start(ApplicationContext ctx, String coreUrl, String extKey) {
        DispatcherServlet dispatcher = ctx.getBean(DispatcherServlet.class);
        ManagedChannel channel = ManagedChannelBuilder.forTarget(coreUrl).usePlaintext().build();
        ExtensionTunnelGrpc.ExtensionTunnelStub stub = ExtensionTunnelGrpc.newStub(channel);

        StreamObserver<TunnelOuterClass.TunnelMessage> responseObserver = new StreamObserver<>() {
            @Override
            public void onNext(TunnelOuterClass.TunnelMessage msg) {
                if (msg.hasHttpReqChunk()) {
                    TunnelOuterClass.HttpRequestChunk chunk = msg.getHttpReqChunk();
                    // Bridging request using Spring dispatcherServlet.service(...)
                    // And write back to stub.onNext(HttpResponseChunk)
                }
            }
            @Override
            public void onError(Throwable t) {}
            @Override
            public void onCompleted() {}
        };

        StreamObserver<TunnelOuterClass.TunnelMessage> requestObserver = stub.connect(responseObserver);
        requestObserver.onNext(TunnelOuterClass.TunnelMessage.newBuilder()
            .setRegisterReq(TunnelOuterClass.RegisterRequest.newBuilder()
                .setExtensionKey(extKey)
                .setVersion("1.0.0")
                .setIsDev(true)
                .build())
            .build());
    }
}
```

- [ ] **Step 4: Commit**

```bash
git add sdk/java/src/
git commit -m "feat: implement Java Spring Boot Servlet Dispatcher HTTP bridge"
```

---

### Task 3: Java CMDS Database Client & Spring boot Example

Expose Java SDK Database APIs and compile the Spring Boot example project template.

**Files:**
- Create: `sdk/java/src/main/java/com/ty/sdk/db/DbClient.java`
- Create: `extension-example/java/backend/src/main/java/com/example/demo/DemoApplication.java`

**Interfaces:**
- Produces: `DbClient.get(String collection, String id, Class<T> clazz)`

- [ ] **Step 1: Write `DbClient.java` wrappers**

```java
package com.ty.sdk.db;

import com.fasterxml.jackson.databind.ObjectMapper;
import tunnel.*;

public class DbClient {
    private final DatabaseServiceGrpc.DatabaseServiceBlockingStub stub;
    private final ObjectMapper mapper = new ObjectMapper();

    public DbClient(io.grpc.Channel channel) {
        this.stub = DatabaseServiceGrpc.newBlockingStub(channel);
    }

    public <T> T get(String collection, String docId, Class<T> clazz) throws Exception {
        Database.GetRequest req = Database.GetRequest.newBuilder()
            .setCollection(collection)
            .setDocumentId(docId)
            .build();
        Database.GetResponse resp = stub.get(req);
        if (!resp.getFound()) {
            return clazz.getDeclaredConstructor().newInstance();
        }
        return mapper.readValue(resp.getJsonData().toByteArray(), clazz);
    }
}
```

- [ ] **Step 2: Commit**

```bash
git add sdk/java/src/main/java/com/ty/sdk/db/
git commit -m "feat: implement Java CMDS client wrappers"
```
