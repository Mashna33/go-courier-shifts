package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"

	"github.com/Mashna33/go-courier-shifts/internal/shifts"
)

// Коды ошибок PostgreSQL, которые мы превращаем в доменные ошибки.
const (
	pgExclusionViolation  = "23P01"
	pgForeignKeyViolation = "23503"
)

// activeStatuses — записи, занимающие место в слоте.
const activeStatuses = `('booked', 'started', 'finished')`

type Repo struct {
	db *sql.DB
}

var _ shifts.Repository = (*Repo)(nil)

func NewRepo(db *sql.DB) *Repo { return &Repo{db: db} }

const bookingCols = `id, slot_id, courier_id, status, started_at, finished_at, earned_kopecks, created_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanBooking(row rowScanner) (shifts.Booking, error) {
	var (
		b        shifts.Booking
		status   string
		started  sql.NullTime
		finished sql.NullTime
		earned   sql.NullInt64
	)
	if err := row.Scan(&b.ID, &b.SlotID, &b.CourierID, &status, &started, &finished, &earned, &b.CreatedAt); err != nil {
		return shifts.Booking{}, err
	}
	b.Status = shifts.BookingStatus(status)
	if started.Valid {
		t := started.Time
		b.StartedAt = &t
	}
	if finished.Valid {
		t := finished.Time
		b.FinishedAt = &t
	}
	if earned.Valid {
		v := earned.Int64
		b.EarnedKopecks = &v
	}
	return b, nil
}

func (r *Repo) CreateCourier(ctx context.Context, name string) (shifts.Courier, error) {
	var c shifts.Courier
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO couriers (name) VALUES ($1) RETURNING id, name, created_at`, name,
	).Scan(&c.ID, &c.Name, &c.CreatedAt)
	if err != nil {
		return shifts.Courier{}, fmt.Errorf("insert courier: %w", err)
	}
	return c, nil
}

func (r *Repo) CreateSlot(ctx context.Context, in shifts.NewSlot) (shifts.Slot, error) {
	var s shifts.Slot
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO slots (zone, starts_at, ends_at, capacity, hourly_rate_kopecks)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, zone, starts_at, ends_at, capacity, hourly_rate_kopecks, created_at`,
		in.Zone, in.StartsAt, in.EndsAt, in.Capacity, in.HourlyRateKopecks,
	).Scan(&s.ID, &s.Zone, &s.StartsAt, &s.EndsAt, &s.Capacity, &s.HourlyRateKopecks, &s.CreatedAt)
	if err != nil {
		return shifts.Slot{}, fmt.Errorf("insert slot: %w", err)
	}
	s.FreePlaces = s.Capacity
	return s, nil
}

func (r *Repo) ListSlots(ctx context.Context, f shifts.SlotFilter) ([]shifts.Slot, error) {
	var to sql.NullTime
	if !f.To.IsZero() {
		to = sql.NullTime{Time: f.To, Valid: true}
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT s.id, s.zone, s.starts_at, s.ends_at, s.capacity, s.hourly_rate_kopecks, s.created_at,
		       COUNT(b.id) FILTER (WHERE b.status IN `+activeStatuses+`) AS booked
		FROM slots s
		LEFT JOIN bookings b ON b.slot_id = s.id
		WHERE ($1::text = '' OR s.zone = $1)
		  AND s.starts_at >= $2
		  AND ($3::timestamptz IS NULL OR s.starts_at < $3)
		GROUP BY s.id
		ORDER BY s.starts_at, s.id
		LIMIT $4`,
		f.Zone, f.From, to, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list slots: %w", err)
	}
	defer rows.Close()

	slots := make([]shifts.Slot, 0)
	for rows.Next() {
		var s shifts.Slot
		if err := rows.Scan(&s.ID, &s.Zone, &s.StartsAt, &s.EndsAt, &s.Capacity,
			&s.HourlyRateKopecks, &s.CreatedAt, &s.Booked); err != nil {
			return nil, fmt.Errorf("scan slot: %w", err)
		}
		s.FreePlaces = s.Capacity - s.Booked
		slots = append(slots, s)
	}
	return slots, rows.Err()
}

// Book записывает курьера на слот.
//
// Чтобы два параллельных запроса не заняли последнее место одновременно, внутри
// транзакции блокируем строку слота (SELECT ... FOR UPDATE): конкурирующие запросы
// на тот же слот выстраиваются в очередь, а запросы на другие слоты не мешают друг другу.
// То, что один курьер не может быть на двух пересекающихся сменах, гарантирует
// EXCLUDE-ограничение в самой таблице bookings.
func (r *Repo) Book(ctx context.Context, slotID, courierID int64, now time.Time) (shifts.Booking, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return shifts.Booking{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		capacity int
		startsAt time.Time
	)
	err = tx.QueryRowContext(ctx,
		`SELECT capacity, starts_at FROM slots WHERE id = $1 FOR UPDATE`, slotID,
	).Scan(&capacity, &startsAt)
	if errors.Is(err, sql.ErrNoRows) {
		return shifts.Booking{}, shifts.ErrNotFound
	}
	if err != nil {
		return shifts.Booking{}, fmt.Errorf("lock slot: %w", err)
	}
	if !now.Before(startsAt) {
		return shifts.Booking{}, shifts.ErrSlotClosed
	}

	var taken int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM bookings WHERE slot_id = $1 AND status IN `+activeStatuses, slotID,
	).Scan(&taken); err != nil {
		return shifts.Booking{}, fmt.Errorf("count bookings: %w", err)
	}
	if taken >= capacity {
		return shifts.Booking{}, shifts.ErrSlotFull
	}

	b, err := scanBooking(tx.QueryRowContext(ctx, `
		INSERT INTO bookings (slot_id, courier_id, status, period)
		SELECT id, $2::bigint, 'booked', tstzrange(starts_at, ends_at)
		FROM slots WHERE id = $1
		RETURNING `+bookingCols, slotID, courierID))
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) {
			switch pqErr.Code {
			case pgExclusionViolation:
				return shifts.Booking{}, shifts.ErrOverlap
			case pgForeignKeyViolation:
				return shifts.Booking{}, shifts.ErrNotFound // курьера не существует
			}
		}
		return shifts.Booking{}, fmt.Errorf("insert booking: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return shifts.Booking{}, fmt.Errorf("commit: %w", err)
	}
	return b, nil
}

func (r *Repo) GetBooking(ctx context.Context, id int64) (shifts.Booking, shifts.Slot, error) {
	var (
		b        shifts.Booking
		s        shifts.Slot
		status   string
		started  sql.NullTime
		finished sql.NullTime
		earned   sql.NullInt64
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT b.id, b.slot_id, b.courier_id, b.status, b.started_at, b.finished_at, b.earned_kopecks, b.created_at,
		       s.id, s.zone, s.starts_at, s.ends_at, s.capacity, s.hourly_rate_kopecks, s.created_at
		FROM bookings b
		JOIN slots s ON s.id = b.slot_id
		WHERE b.id = $1`, id,
	).Scan(&b.ID, &b.SlotID, &b.CourierID, &status, &started, &finished, &earned, &b.CreatedAt,
		&s.ID, &s.Zone, &s.StartsAt, &s.EndsAt, &s.Capacity, &s.HourlyRateKopecks, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return shifts.Booking{}, shifts.Slot{}, shifts.ErrNotFound
	}
	if err != nil {
		return shifts.Booking{}, shifts.Slot{}, fmt.Errorf("get booking: %w", err)
	}
	b.Status = shifts.BookingStatus(status)
	if started.Valid {
		t := started.Time
		b.StartedAt = &t
	}
	if finished.Valid {
		t := finished.Time
		b.FinishedAt = &t
	}
	if earned.Valid {
		v := earned.Int64
		b.EarnedKopecks = &v
	}
	return b, s, nil
}

func (r *Repo) Cancel(ctx context.Context, id int64) (shifts.Booking, error) {
	return r.transition(ctx, id, `
		UPDATE bookings SET status = 'cancelled'
		WHERE id = $1 AND status = 'booked'
		RETURNING `+bookingCols, id)
}

func (r *Repo) Start(ctx context.Context, id int64, at time.Time) (shifts.Booking, error) {
	return r.transition(ctx, id, `
		UPDATE bookings SET status = 'started', started_at = $2
		WHERE id = $1 AND status = 'booked'
		RETURNING `+bookingCols, id, at)
}

func (r *Repo) Finish(ctx context.Context, id int64, at time.Time, earnedKopecks int64) (shifts.Booking, error) {
	return r.transition(ctx, id, `
		UPDATE bookings SET status = 'finished', finished_at = $2, earned_kopecks = $3
		WHERE id = $1 AND status = 'started'
		RETURNING `+bookingCols, id, at, earnedKopecks)
}

// transition выполняет условный UPDATE. Если ни одна строка не изменилась,
// различаем «записи нет» и «запись есть, но статус не подходит».
func (r *Repo) transition(ctx context.Context, id int64, query string, args ...any) (shifts.Booking, error) {
	b, err := scanBooking(r.db.QueryRowContext(ctx, query, args...))
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return shifts.Booking{}, fmt.Errorf("update booking: %w", err)
	}
	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM bookings WHERE id = $1)`, id,
	).Scan(&exists); err != nil {
		return shifts.Booking{}, fmt.Errorf("check booking: %w", err)
	}
	if !exists {
		return shifts.Booking{}, shifts.ErrNotFound
	}
	return shifts.Booking{}, shifts.ErrInvalidState
}

func (r *Repo) ListCourierBookings(ctx context.Context, courierID int64) ([]shifts.Booking, error) {
	var exists bool
	if err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM couriers WHERE id = $1)`, courierID,
	).Scan(&exists); err != nil {
		return nil, fmt.Errorf("check courier: %w", err)
	}
	if !exists {
		return nil, shifts.ErrNotFound
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+bookingCols+` FROM bookings WHERE courier_id = $1 ORDER BY created_at DESC, id DESC`, courierID)
	if err != nil {
		return nil, fmt.Errorf("list bookings: %w", err)
	}
	defer rows.Close()

	out := make([]shifts.Booking, 0)
	for rows.Next() {
		b, err := scanBooking(rows)
		if err != nil {
			return nil, fmt.Errorf("scan booking: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
