# Build from the repository root:
#   docker build -f extension-example/java/Dockerfile -t java-example .
# The frontend dist is bundled in; on startup the SDK streams it to Core. The
# Java SDK is built and installed first so the example can resolve it. The
# container opens no port.

# 1. Build the micro-frontend.
FROM node:20-alpine AS frontend
WORKDIR /fe
COPY extension-example/java/frontend/package.json ./
RUN npm install --no-audit --no-fund
COPY extension-example/java/frontend/ ./
RUN npm run build

# 2. Build the Java SDK (proto resolved at ../../proto) then the example fat jar.
FROM maven:3.9-eclipse-temurin-17 AS backend
WORKDIR /src
COPY proto /src/proto
COPY sdk/java /src/sdk/java
RUN mvn -q -B -f /src/sdk/java/pom.xml install -DskipTests
COPY extension-example/java/backend /src/extension-example/java/backend
RUN mvn -q -B -f /src/extension-example/java/backend/pom.xml package -DskipTests

# 3. Runtime: JRE + fat jar + built frontend + manifest.
FROM eclipse-temurin:17-jre
WORKDIR /app
COPY --from=backend /src/extension-example/java/backend/target/java-extension-example-1.0.0.jar /app/app.jar
COPY --from=frontend /fe/dist /app/frontend
COPY extension-example/java/manifest.yaml /app/manifest.yaml
ENV CORE_URL=core:9000 \
    FRONTEND_DIR=/app/frontend \
    MANIFEST_PATH=/app/manifest.yaml
ENTRYPOINT ["java", "-jar", "/app/app.jar"]
