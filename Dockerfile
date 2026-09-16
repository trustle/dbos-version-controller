FROM ghcr.io/astral-sh/uv:0.11.3 AS uv

FROM python:3.13-slim AS build

COPY --from=uv /uv /uvx /bin/

# uv build hygiene:
# - copy (not hardlink) from the cache: the BuildKit cache mount and the venv
#   live on different filesystems, so hardlinking falls back to a slow copy.
# - compile bytecode at build time so the container does not pay it on cold start.
ENV UV_LINK_MODE=copy \
    UV_COMPILE_BYTECODE=1

WORKDIR /app

COPY pyproject.toml uv.lock README.md ./
COPY src/ ./src/

# --no-dev: skip development dependencies for a smaller image.
# --frozen: use the exact versions in the lock file, with no re-resolution.
RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --frozen --no-dev

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
