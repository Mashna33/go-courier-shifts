package shifts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRepo — заглушка хранилища: возвращает заранее заданные значения и запоминает вызовы.
type fakeRepo struct {
	booking Booking
	slot    Slot
	getErr  error

	createdSlot NewSlot
	slotCreated bool
	listFilter  SlotFilter

	cancelled bool
	startedAt time.Time
	finishAt  time.Time
	earned    int64
}

var _ Repository = (*fakeRepo)(nil)

func (f *fakeRepo) CreateCourier(_ context.Context, name string) (Courier, error) {
	return Courier{ID: 1, Name: name}, nil
}

func (f *fakeRepo) CreateSlot(_ context.Context, s NewSlot) (Slot, error) {
	f.createdSlot, f.slotCreated = s, true
	return Slot{ID: 1, Zone: s.Zone, StartsAt: s.StartsAt, EndsAt: s.EndsAt, Capacity: s.Capacity}, nil
}

func (f *fakeRepo) ListSlots(_ context.Context, flt SlotFilter) ([]Slot, error) {
	f.listFilter = flt
	return nil, nil
}

func (f *fakeRepo) Book(context.Context, int64, int64, time.Time) (Booking, error) {
	return Booking{ID: 1, Status: StatusBooked}, nil
}

func (f *fakeRepo) GetBooking(context.Context, int64) (Booking, Slot, error) {
	return f.booking, f.slot, f.getErr
}

func (f *fakeRepo) Cancel(context.Context, int64) (Booking, error) {
	f.cancelled = true
	return Booking{Status: StatusCancelled}, nil
}

func (f *fakeRepo) Start(_ context.Context, _ int64, at time.Time) (Booking, error) {
	f.startedAt = at
	return Booking{Status: StatusStarted, StartedAt: &at}, nil
}

func (f *fakeRepo) Finish(_ context.Context, _ int64, at time.Time, earned int64) (Booking, error) {
	f.finishAt, f.earned = at, earned
	return Booking{Status: StatusFinished, EarnedKopecks: &earned}, nil
}

func (f *fakeRepo) ListCourierBookings(context.Context, int64) ([]Booking, error) { return nil, nil }

var (
	// «Сейчас» в тестах фиксировано, чтобы результаты не зависели от времени запуска.
	t0        = time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	slotStart = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	slotEnd   = slotStart.Add(4 * time.Hour)
)

func newTestService(repo *fakeRepo, now time.Time) *Service {
	return NewService(repo, func() time.Time { return now })
}

func TestCreateCourierValidation(t *testing.T) {
	svc := newTestService(&fakeRepo{}, t0)

	for _, name := range []string{"", "   ", strings.Repeat("я", maxNameLen+1)} {
		if _, err := svc.CreateCourier(context.Background(), name); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("name %q: want ErrInvalidInput, got %v", name, err)
		}
	}

	c, err := svc.CreateCourier(context.Background(), "  Иван  ")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Иван" {
		t.Errorf("name must be trimmed, got %q", c.Name)
	}
}

func TestCreateSlotValidation(t *testing.T) {
	valid := NewSlot{Zone: "center", StartsAt: slotStart, EndsAt: slotEnd, Capacity: 5, HourlyRateKopecks: 30000}

	tests := []struct {
		name   string
		mutate func(*NewSlot)
	}{
		{"пустая зона", func(s *NewSlot) { s.Zone = "  " }},
		{"слишком длинная зона", func(s *NewSlot) { s.Zone = strings.Repeat("z", maxZoneLen+1) }},
		{"конец равен началу", func(s *NewSlot) { s.EndsAt = s.StartsAt }},
		{"конец раньше начала", func(s *NewSlot) { s.EndsAt = s.StartsAt.Add(-time.Hour) }},
		{"слот длиннее максимума", func(s *NewSlot) { s.EndsAt = s.StartsAt.Add(MaxSlotDuration + time.Minute) }},
		{"начало в прошлом", func(s *NewSlot) { s.StartsAt = t0.Add(-time.Minute) }},
		{"нулевая вместимость", func(s *NewSlot) { s.Capacity = 0 }},
		{"слишком большая вместимость", func(s *NewSlot) { s.Capacity = MaxCapacity + 1 }},
		{"отрицательная ставка", func(s *NewSlot) { s.HourlyRateKopecks = -1 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{}
			in := valid
			tt.mutate(&in)
			if _, err := newTestService(repo, t0).CreateSlot(context.Background(), in); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("want ErrInvalidInput, got %v", err)
			}
			if repo.slotCreated {
				t.Fatal("repository must not be called for invalid input")
			}
		})
	}

	t.Run("корректный слот сохраняется в UTC", func(t *testing.T) {
		repo := &fakeRepo{}
		local := time.FixedZone("NSK", 7*3600)
		in := valid
		in.StartsAt, in.EndsAt = slotStart.In(local), slotEnd.In(local)
		if _, err := newTestService(repo, t0).CreateSlot(context.Background(), in); err != nil {
			t.Fatal(err)
		}
		if repo.createdSlot.StartsAt.Location() != time.UTC {
			t.Errorf("starts_at must be stored in UTC, got %v", repo.createdSlot.StartsAt.Location())
		}
	})
}

func TestListSlotsDefaultsAndValidation(t *testing.T) {
	repo := &fakeRepo{}
	svc := newTestService(repo, t0)

	if _, err := svc.ListSlots(context.Background(), SlotFilter{}); err != nil {
		t.Fatal(err)
	}
	if !repo.listFilter.From.Equal(t0) {
		t.Errorf("from must default to now, got %v", repo.listFilter.From)
	}
	if repo.listFilter.Limit != DefaultListLimit {
		t.Errorf("limit must default to %d, got %d", DefaultListLimit, repo.listFilter.Limit)
	}

	bad := []SlotFilter{
		{Limit: -1},
		{Limit: MaxListLimit + 1},
		{From: t0, To: t0},
		{From: t0, To: t0.Add(-time.Hour)},
	}
	for _, f := range bad {
		if _, err := svc.ListSlots(context.Background(), f); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("filter %+v: want ErrInvalidInput, got %v", f, err)
		}
	}
}

func TestCancel(t *testing.T) {
	booked := Booking{ID: 1, Status: StatusBooked}
	slot := Slot{StartsAt: slotStart, EndsAt: slotEnd}

	tests := []struct {
		name    string
		booking Booking
		now     time.Time
		getErr  error
		wantErr error
	}{
		{"до начала слота можно отменить", booked, slotStart.Add(-time.Minute), nil, nil},
		{"в момент начала слота уже поздно", booked, slotStart, nil, ErrCancelTooLate},
		{"после начала слота поздно", booked, slotStart.Add(time.Hour), nil, ErrCancelTooLate},
		{"уже отменённую отменить нельзя", Booking{Status: StatusCancelled}, t0, nil, ErrInvalidState},
		{"начатую отменить нельзя", Booking{Status: StatusStarted}, t0, nil, ErrInvalidState},
		{"записи нет", booked, t0, ErrNotFound, ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{booking: tt.booking, slot: slot, getErr: tt.getErr}
			_, err := newTestService(repo, tt.now).Cancel(context.Background(), 1)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("want %v, got %v", tt.wantErr, err)
			}
			if (tt.wantErr == nil) != repo.cancelled {
				t.Fatalf("repo.Cancel called = %v, want %v", repo.cancelled, tt.wantErr == nil)
			}
		})
	}
}

func TestStartWindow(t *testing.T) {
	booked := Booking{ID: 1, Status: StatusBooked}
	slot := Slot{StartsAt: slotStart, EndsAt: slotEnd}

	tests := []struct {
		name    string
		booking Booking
		now     time.Time
		wantErr error
	}{
		{"слишком рано", booked, slotStart.Add(-StartWindowBefore - time.Second), ErrOutsideStartWindow},
		{"ровно за 15 минут до начала", booked, slotStart.Add(-StartWindowBefore), nil},
		{"в середине слота", booked, slotStart.Add(time.Hour), nil},
		{"в момент конца слота уже нельзя", booked, slotEnd, ErrOutsideStartWindow},
		{"смена уже начата", Booking{Status: StatusStarted}, slotStart, ErrInvalidState},
		{"смена отменена", Booking{Status: StatusCancelled}, slotStart, ErrInvalidState},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeRepo{booking: tt.booking, slot: slot}
			_, err := newTestService(repo, tt.now).Start(context.Background(), 1)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("want %v, got %v", tt.wantErr, err)
			}
			if tt.wantErr == nil && !repo.startedAt.Equal(tt.now) {
				t.Fatalf("started_at = %v, want %v", repo.startedAt, tt.now)
			}
		})
	}
}

func TestFinishCalculatesEarnings(t *testing.T) {
	startedAt := slotStart.Add(30 * time.Minute)
	slot := Slot{StartsAt: slotStart, EndsAt: slotEnd, HourlyRateKopecks: 30000}
	started := Booking{ID: 1, Status: StatusStarted, StartedAt: &startedAt}

	t.Run("заработок за отработанное время", func(t *testing.T) {
		repo := &fakeRepo{booking: started, slot: slot}
		now := slotStart.Add(2*time.Hour + 30*time.Minute) // отработано 2 часа
		if _, err := newTestService(repo, now).Finish(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if repo.earned != 60000 {
			t.Fatalf("earned = %d, want 60000", repo.earned)
		}
	})

	t.Run("после конца слота оплачивается только время слота", func(t *testing.T) {
		repo := &fakeRepo{booking: started, slot: slot}
		if _, err := newTestService(repo, slotEnd.Add(3*time.Hour)).Finish(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if repo.earned != 105000 { // с 10:30 до 14:00 = 3,5 часа
			t.Fatalf("earned = %d, want 105000", repo.earned)
		}
	})

	t.Run("нельзя завершить смену, которая не начата", func(t *testing.T) {
		repo := &fakeRepo{booking: Booking{Status: StatusBooked}, slot: slot}
		if _, err := newTestService(repo, slotEnd).Finish(context.Background(), 1); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("want ErrInvalidState, got %v", err)
		}
	})
}
