-- Нужен для EXCLUDE-ограничения по сочетанию «равенство + пересечение диапазонов».
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE couriers (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT        NOT NULL CHECK (length(btrim(name)) > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE slots (
    id                  BIGSERIAL PRIMARY KEY,
    zone                TEXT        NOT NULL CHECK (length(btrim(zone)) > 0),
    starts_at           TIMESTAMPTZ NOT NULL,
    ends_at             TIMESTAMPTZ NOT NULL,
    capacity            INTEGER     NOT NULL CHECK (capacity > 0),
    hourly_rate_kopecks BIGINT      NOT NULL CHECK (hourly_rate_kopecks >= 0),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);

CREATE INDEX slots_zone_starts_idx ON slots (zone, starts_at);
CREATE INDEX slots_starts_idx ON slots (starts_at);

CREATE TABLE bookings (
    id             BIGSERIAL PRIMARY KEY,
    slot_id        BIGINT      NOT NULL REFERENCES slots (id),
    courier_id     BIGINT      NOT NULL REFERENCES couriers (id),
    status         TEXT        NOT NULL CHECK (status IN ('booked', 'cancelled', 'started', 'finished')),
    -- Копия интервала слота: по ней база сама запрещает курьеру две смены одновременно.
    period         TSTZRANGE   NOT NULL,
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    earned_kopecks BIGINT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    EXCLUDE USING gist (courier_id WITH =, period WITH &&) WHERE (status IN ('booked', 'started'))
);

CREATE INDEX bookings_slot_idx ON bookings (slot_id);
CREATE INDEX bookings_courier_idx ON bookings (courier_id, created_at DESC);
