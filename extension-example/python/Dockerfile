# Build from the repository root:
#   docker build -f extension-example/python/Dockerfile -t python-example .
# The frontend dist is bundled in; on startup the SDK streams it to Core. The
# repo layout is preserved under /app so the backend's relative SDK import
# (../../../sdk/python) resolves. The container opens no port.

# 1. Build the micro-frontend.
FROM node:20-alpine AS frontend
WORKDIR /fe
COPY extension-example/python/frontend/package.json ./
RUN npm install --no-audit --no-fund
COPY extension-example/python/frontend/ ./
RUN npm run build

# 2. Runtime: Python backend + SDK + built frontend.
FROM python:3.11-slim
WORKDIR /app
COPY sdk/python/requirements.txt /app/sdk/python/requirements.txt
RUN pip install --no-cache-dir -r /app/sdk/python/requirements.txt
COPY sdk/python /app/sdk/python
COPY extension-example/python/backend /app/extension-example/python/backend
COPY extension-example/python/manifest.yaml /app/extension-example/python/manifest.yaml
COPY --from=frontend /fe/dist /app/frontend
ENV CORE_URL=core:9000 \
    FRONTEND_DIR=/app/frontend \
    PYTHONUNBUFFERED=1
WORKDIR /app/extension-example/python/backend
CMD ["python", "main.py"]
