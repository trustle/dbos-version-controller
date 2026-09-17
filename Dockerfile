FROM ghcr.io/astral-sh/uv:0.11.3 AS uv

FROM python:3.13-slim AS build

COPY --from=uv /uv /uvx /bin/

# Compile bytecode at build time so the container does not pay it on cold start.
ENV UV_COMPILE_BYTECODE=1

WORKDIR /app

COPY pyproject.toml uv.lock README.md ./
COPY src/ ./src/

# --no-dev: skip development dependencies for a smaller image.
# --frozen: use the exact versions in the lock file, with no re-resolution.
# No BuildKit cache mount: the image must also build under the legacy builder
# that Google Cloud Build's docker step uses.
RUN uv sync --frozen --no-dev --no-cache

FROM python:3.13-slim

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ARG UGID=1000
RUN useradd -m -u ${UGID} controller

WORKDIR /app
COPY --from=build --chown=${UGID}:${UGID} /app /app

USER ${UGID}

ENV PYTHONUNBUFFERED=1
ENV PATH="/app/.venv/bin:$PATH"

ENTRYPOINT ["dbos-version-controller"]
