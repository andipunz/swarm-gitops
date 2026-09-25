# Build
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /swarm-gitops ./cmd/swarm-gitops

# Runtime: the docker CLI does `stack config` / `stack deploy` exactly like an admin would.
FROM docker:29-cli
COPY --from=build /swarm-gitops /usr/local/bin/swarm-gitops
COPY deploy/policy.yml /etc/swarm-gitops/policy.yml
ENV POLICY_FILE=/etc/swarm-gitops/policy.yml \
    DATA_DIR=/data
ENTRYPOINT ["swarm-gitops"]
CMD ["serve"]
