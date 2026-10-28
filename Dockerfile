# syntax=docker/dockerfile:1

# --- Сборка ---
FROM golang:1.24-alpine AS build
WORKDIR /src

# Сначала зависимости: этот слой кэшируется, пока go.mod и go.sum не менялись.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/shifts ./cmd/shifts

# --- Запуск ---
# distroless: в образе нет оболочки и пакетного менеджера, процесс работает не от root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/shifts /shifts
EXPOSE 8080
ENTRYPOINT ["/shifts"]
