package postgres_test

// Интеграционные тесты работают с настоящей PostgreSQL. Они запускаются, только если задана
// переменная TEST_DATABASE_URL, например:
//
//	TEST_DATABASE_URL="postgres://shifts:shifts@localhost:5432/shifts_test?sslmode=disable" go test ./...
//
// Пользователю нужны права создавать расширения (btree_gist). Перед каждым тестом таблицы очищаются,
// поэтому используйте отдельную тестовую базу.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/Mashna33/go-courier-shifts/internal/postgres"
	"github.com/Mashna33/go-courier-shifts/internal/shifts"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set; skipping integration test")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(30)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()
	if err := postgres.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `TRUNCATE bookings, slots, couriers RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return db
}

// Все слоты создаём в далёком будущем относительно реального времени и подставляем «часы» сервиса.
var base = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func at(hour int) time.Time { return base.Add(time.Duration(hour) * time.Hour) }

func mustSlot(t *testing.T, repo *postgres.Repo, zone string, from, to, capacity int) shifts.Slot {
	t.Helper()
	s, err := repo.CreateSlot(context.Background(), shifts.NewSlot{
		Zone: zone, StartsAt: at(from), EndsAt: at(to), Capacity: capacity, HourlyRateKopecks: 30000,
	})
	if err != nil {
		t.Fatalf("create slot: %v", err)
	}
	return s
}

func mustCourier(t *testing.T, repo *postgres.Repo, name string) shifts.Courier {
	t.Helper()
	c, err := repo.CreateCourier(context.Background(), name)
	if err != nil {
		t.Fatalf("create courier: %v", err)
	}
	return c
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < 2; i++ {
		if err := postgres.Migrate(context.Background(), db); err != nil {
			t.Fatalf("migrate #%d: %v", i+1, err)
		}
	}
}

// Главная проверка корректности: при гонке за последние места мест не должно оказаться больше, чем есть.
func TestConcurrentBookingNeverOversells(t *testing.T) {
	db := openTestDB(t)
	repo := postgres.NewRepo(db)
	ctx := context.Background()

	const capacity, couriers = 3, 25
	slot := mustSlot(t, repo, "center", 10, 14, capacity)

	ids := make([]int64, couriers)
	for i := range ids {
		ids[i] = mustCourier(t, repo, fmt.Sprintf("courier-%d", i)).ID
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
		full    int
		other   []error
		start   = make(chan struct{})
	)
	for _, id := range ids {
		wg.Add(1)
		go func(courierID int64) {
			defer wg.Done()
			<-start // все горутины стартуют одновременно
			_, err := repo.Book(ctx, slot.ID, courierID, at(0))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, shifts.ErrSlotFull):
				full++
			default:
				other = append(other, err)
			}
		}(id)
	}
	close(start)
	wg.Wait()

	if len(other) > 0 {
		t.Fatalf("unexpected errors: %v", other)
	}
	if success != capacity || full != couriers-capacity {
		t.Fatalf("booked=%d full=%d, want booked=%d full=%d", success, full, capacity, couriers-capacity)
	}

	slots, err := repo.ListSlots(ctx, shifts.SlotFilter{From: at(0), Limit: 10})
	if err != nil || len(slots) != 1 {
		t.Fatalf("list slots: %v (%d slots)", err, len(slots))
	}
	if slots[0].Booked != capacity || slots[0].FreePlaces != 0 {
		t.Fatalf("slot counters: booked=%d free=%d", slots[0].Booked, slots[0].FreePlaces)
	}
}

func TestOverlappingShiftsAreRejectedByDatabase(t *testing.T) {
	db := openTestDB(t)
	repo := postgres.NewRepo(db)
	ctx := context.Background()

	courier := mustCourier(t, repo, "Иван")
	morning := mustSlot(t, repo, "center", 10, 14, 5)
	overlapping := mustSlot(t, repo, "north", 12, 16, 5)
	adjacent := mustSlot(t, repo, "south", 14, 18, 5)

	first, err := repo.Book(ctx, morning.ID, courier.ID, at(0))
	if err != nil {
		t.Fatalf("first booking: %v", err)
	}
	if _, err := repo.Book(ctx, morning.ID, courier.ID, at(0)); !errors.Is(err, shifts.ErrOverlap) {
		t.Errorf("second booking of the same slot: want ErrOverlap, got %v", err)
	}
	if _, err := repo.Book(ctx, overlapping.ID, courier.ID, at(0)); !errors.Is(err, shifts.ErrOverlap) {
		t.Errorf("overlapping slot: want ErrOverlap, got %v", err)
	}
	if _, err := repo.Book(ctx, adjacent.ID, courier.ID, at(0)); err != nil {
		t.Errorf("slot starting exactly when the previous ends must be allowed: %v", err)
	}

	// После отмены первой смены время освобождается: тот же слот снова можно занять.
	// (Слот 12–16 по-прежнему пересекается со сменой 14–18, которая осталась в силе.)
	if _, err := repo.Cancel(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Book(ctx, morning.ID, courier.ID, at(0)); err != nil {
		t.Errorf("booking the freed time again: %v", err)
	}
	if _, err := repo.Book(ctx, overlapping.ID, courier.ID, at(0)); !errors.Is(err, shifts.ErrOverlap) {
		t.Errorf("12-16 still overlaps the active 14-18 shift: want ErrOverlap, got %v", err)
	}
}

func TestCancelledBookingFreesThePlace(t *testing.T) {
	db := openTestDB(t)
	repo := postgres.NewRepo(db)
	ctx := context.Background()

	slot := mustSlot(t, repo, "center", 10, 14, 1)
	a, b := mustCourier(t, repo, "A"), mustCourier(t, repo, "B")

	booking, err := repo.Book(ctx, slot.ID, a.ID, at(0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Book(ctx, slot.ID, b.ID, at(0)); !errors.Is(err, shifts.ErrSlotFull) {
		t.Fatalf("want ErrSlotFull, got %v", err)
	}
	if _, err := repo.Cancel(ctx, booking.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Book(ctx, slot.ID, b.ID, at(0)); err != nil {
		t.Fatalf("place must be free after cancellation: %v", err)
	}
}

func TestBookErrors(t *testing.T) {
	db := openTestDB(t)
	repo := postgres.NewRepo(db)
	ctx := context.Background()

	slot := mustSlot(t, repo, "center", 10, 14, 2)
	courier := mustCourier(t, repo, "Иван")

	if _, err := repo.Book(ctx, 999, courier.ID, at(0)); !errors.Is(err, shifts.ErrNotFound) {
		t.Errorf("unknown slot: want ErrNotFound, got %v", err)
	}
	if _, err := repo.Book(ctx, slot.ID, 999, at(0)); !errors.Is(err, shifts.ErrNotFound) {
		t.Errorf("unknown courier: want ErrNotFound, got %v", err)
	}
	if _, err := repo.Book(ctx, slot.ID, courier.ID, at(10)); !errors.Is(err, shifts.ErrSlotClosed) {
		t.Errorf("slot already started: want ErrSlotClosed, got %v", err)
	}
	// Неудачные попытки не должны занимать места.
	slots, err := repo.ListSlots(ctx, shifts.SlotFilter{From: at(0), Limit: 10})
	if err != nil || len(slots) != 1 || slots[0].Booked != 0 {
		t.Fatalf("failed bookings left traces: %v %+v", err, slots)
	}
}

// Две одновременные попытки начать одну смену: сработать должна ровно одна.
func TestConcurrentStartSucceedsOnce(t *testing.T) {
	db := openTestDB(t)
	repo := postgres.NewRepo(db)
	ctx := context.Background()

	slot := mustSlot(t, repo, "center", 10, 14, 1)
	courier := mustCourier(t, repo, "Иван")
	booking, err := repo.Book(ctx, slot.ID, courier.ID, at(0))
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
		invalid int
		other   []error
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.Start(ctx, booking.ID, at(10))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, shifts.ErrInvalidState):
				invalid++
			default:
				other = append(other, err)
			}
		}()
	}
	wg.Wait()

	if len(other) > 0 || success != 1 || invalid != attempts-1 {
		t.Fatalf("success=%d invalid=%d other=%v", success, invalid, other)
	}
	if _, err := repo.Start(ctx, 12345, at(10)); !errors.Is(err, shifts.ErrNotFound) {
		t.Errorf("unknown booking: want ErrNotFound, got %v", err)
	}
}

// Полный сценарий через бизнес-логику и настоящую базу, с управляемыми часами.
func TestFullShiftLifecycle(t *testing.T) {
	db := openTestDB(t)
	repo := postgres.NewRepo(db)
	ctx := context.Background()

	now := at(-48) // «сейчас» двигаем вручную
	svc := shifts.NewService(repo, func() time.Time { return now })

	courier, err := svc.CreateCourier(ctx, "Иван")
	if err != nil {
		t.Fatal(err)
	}
	slot, err := svc.CreateSlot(ctx, shifts.NewSlot{
		Zone: "center", StartsAt: at(10), EndsAt: at(14), Capacity: 2, HourlyRateKopecks: 30000,
	})
	if err != nil {
		t.Fatal(err)
	}

	booking, err := svc.Book(ctx, slot.ID, courier.ID)
	if err != nil || booking.Status != shifts.StatusBooked {
		t.Fatalf("book: %v %+v", err, booking)
	}

	now = at(8) // слишком рано для старта
	if _, err := svc.Start(ctx, booking.ID); !errors.Is(err, shifts.ErrOutsideStartWindow) {
		t.Fatalf("early start: want ErrOutsideStartWindow, got %v", err)
	}

	now = at(10).Add(30 * time.Minute) // курьер опоздал на полчаса
	started, err := svc.Start(ctx, booking.ID)
	if err != nil || started.Status != shifts.StatusStarted || started.StartedAt == nil {
		t.Fatalf("start: %v %+v", err, started)
	}
	if _, err := svc.Cancel(ctx, booking.ID); !errors.Is(err, shifts.ErrCancelTooLate) &&
		!errors.Is(err, shifts.ErrInvalidState) {
		t.Fatalf("cancel of started shift must fail, got %v", err)
	}

	now = at(12).Add(30 * time.Minute) // отработано ровно два часа
	finished, err := svc.Finish(ctx, booking.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != shifts.StatusFinished || finished.EarnedKopecks == nil || *finished.EarnedKopecks != 60000 {
		t.Fatalf("finish: %+v", finished)
	}
	if _, err := svc.Finish(ctx, booking.ID); !errors.Is(err, shifts.ErrInvalidState) {
		t.Fatalf("second finish: want ErrInvalidState, got %v", err)
	}

	list, err := svc.CourierBookings(ctx, courier.ID)
	if err != nil || len(list) != 1 || list[0].Status != shifts.StatusFinished {
		t.Fatalf("courier bookings: %v %+v", err, list)
	}
	if _, err := svc.CourierBookings(ctx, 999); !errors.Is(err, shifts.ErrNotFound) {
		t.Fatalf("unknown courier: want ErrNotFound, got %v", err)
	}
}

func TestListSlotsFilters(t *testing.T) {
	db := openTestDB(t)
	repo := postgres.NewRepo(db)
	ctx := context.Background()

	mustSlot(t, repo, "center", 10, 12, 1)
	mustSlot(t, repo, "center", 13, 15, 1)
	mustSlot(t, repo, "north", 10, 12, 1)

	tests := []struct {
		name string
		f    shifts.SlotFilter
		want int
	}{
		{"все", shifts.SlotFilter{From: at(0), Limit: 10}, 3},
		{"по зоне", shifts.SlotFilter{Zone: "center", From: at(0), Limit: 10}, 2},
		{"с границей from", shifts.SlotFilter{From: at(11), Limit: 10}, 1},
		{"с границей to (не включая)", shifts.SlotFilter{From: at(0), To: at(13), Limit: 10}, 2},
		{"лимит", shifts.SlotFilter{From: at(0), Limit: 1}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repo.ListSlots(ctx, tt.f)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.want {
				t.Fatalf("got %d slots, want %d", len(got), tt.want)
			}
		})
	}
}
