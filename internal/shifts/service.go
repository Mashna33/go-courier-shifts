package shifts

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// StartWindowBefore — за сколько до начала слота курьер уже может начать смену.
	StartWindowBefore = 15 * time.Minute
	MaxSlotDuration   = 12 * time.Hour
	MaxCapacity       = 1000
	DefaultListLimit  = 50
	MaxListLimit      = 100

	maxNameLen = 100
	maxZoneLen = 64
)

// Clock возвращает текущее время. В тестах подставляем управляемые часы.
type Clock func() time.Time

// Service реализует бизнес-правила поверх хранилища.
type Service struct {
	repo Repository
	now  Clock
}

func NewService(repo Repository, now Clock) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, now: now}
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

func (s *Service) CreateCourier(ctx context.Context, name string) (Courier, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxNameLen {
		return Courier{}, invalid("name must be 1..%d characters", maxNameLen)
	}
	return s.repo.CreateCourier(ctx, name)
}

func (s *Service) CreateSlot(ctx context.Context, in NewSlot) (Slot, error) {
	in.Zone = strings.TrimSpace(in.Zone)
	if in.Zone == "" || utf8.RuneCountInString(in.Zone) > maxZoneLen {
		return Slot{}, invalid("zone must be 1..%d characters", maxZoneLen)
	}
	if !in.EndsAt.After(in.StartsAt) {
		return Slot{}, invalid("ends_at must be after starts_at")
	}
	if in.EndsAt.Sub(in.StartsAt) > MaxSlotDuration {
		return Slot{}, invalid("slot can not be longer than %s", MaxSlotDuration)
	}
	if !in.StartsAt.After(s.now()) {
		return Slot{}, invalid("starts_at must be in the future")
	}
	if in.Capacity < 1 || in.Capacity > MaxCapacity {
		return Slot{}, invalid("capacity must be 1..%d", MaxCapacity)
	}
	if in.HourlyRateKopecks < 0 {
		return Slot{}, invalid("hourly_rate_kopecks must not be negative")
	}
	in.StartsAt, in.EndsAt = in.StartsAt.UTC(), in.EndsAt.UTC()
	return s.repo.CreateSlot(ctx, in)
}

func (s *Service) ListSlots(ctx context.Context, f SlotFilter) ([]Slot, error) {
	if f.From.IsZero() {
		f.From = s.now()
	}
	if !f.To.IsZero() && !f.To.After(f.From) {
		return nil, invalid("'to' must be after 'from'")
	}
	switch {
	case f.Limit == 0:
		f.Limit = DefaultListLimit
	case f.Limit < 0 || f.Limit > MaxListLimit:
		return nil, invalid("limit must be 1..%d", MaxListLimit)
	}
	return s.repo.ListSlots(ctx, f)
}

func (s *Service) Book(ctx context.Context, slotID, courierID int64) (Booking, error) {
	return s.repo.Book(ctx, slotID, courierID, s.now())
}

// Cancel отменяет запись, пока слот ещё не начался.
func (s *Service) Cancel(ctx context.Context, bookingID int64) (Booking, error) {
	b, slot, err := s.repo.GetBooking(ctx, bookingID)
	if err != nil {
		return Booking{}, err
	}
	if b.Status != StatusBooked {
		return Booking{}, ErrInvalidState
	}
	if !s.now().Before(slot.StartsAt) {
		return Booking{}, ErrCancelTooLate
	}
	return s.repo.Cancel(ctx, bookingID)
}

// Start начинает смену: можно за StartWindowBefore до начала слота и до его конца.
func (s *Service) Start(ctx context.Context, bookingID int64) (Booking, error) {
	b, slot, err := s.repo.GetBooking(ctx, bookingID)
	if err != nil {
		return Booking{}, err
	}
	if b.Status != StatusBooked {
		return Booking{}, ErrInvalidState
	}
	now := s.now()
	if now.Before(slot.StartsAt.Add(-StartWindowBefore)) || !now.Before(slot.EndsAt) {
		return Booking{}, ErrOutsideStartWindow
	}
	return s.repo.Start(ctx, bookingID, now.UTC())
}

// Finish завершает смену и считает заработок.
func (s *Service) Finish(ctx context.Context, bookingID int64) (Booking, error) {
	b, slot, err := s.repo.GetBooking(ctx, bookingID)
	if err != nil {
		return Booking{}, err
	}
	if b.Status != StatusStarted || b.StartedAt == nil {
		return Booking{}, ErrInvalidState
	}
	now := s.now()
	earned := CalcEarnings(slot.HourlyRateKopecks, slot.StartsAt, slot.EndsAt, *b.StartedAt, now)
	return s.repo.Finish(ctx, bookingID, now.UTC(), earned)
}

func (s *Service) CourierBookings(ctx context.Context, courierID int64) ([]Booking, error) {
	return s.repo.ListCourierBookings(ctx, courierID)
}
