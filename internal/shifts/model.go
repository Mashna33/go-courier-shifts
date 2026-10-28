// Package shifts содержит доменную модель сервиса смен курьеров и бизнес-правила.
// Пакет не знает ни про HTTP, ни про PostgreSQL: хранилище подключается через интерфейс Repository.
package shifts

import (
	"context"
	"errors"
	"time"
)

// Доменные ошибки. HTTP-слой сопоставляет их со статусами ответа.
var (
	ErrInvalidInput       = errors.New("invalid input")
	ErrNotFound           = errors.New("not found")
	ErrSlotFull           = errors.New("slot is full")
	ErrSlotClosed         = errors.New("slot has already started")
	ErrOverlap            = errors.New("courier already has a shift in this time range")
	ErrInvalidState       = errors.New("booking is in a state that does not allow this action")
	ErrCancelTooLate      = errors.New("booking can be cancelled only before the slot starts")
	ErrOutsideStartWindow = errors.New("shift can be started only shortly before or during the slot")
)

// BookingStatus описывает жизненный цикл записи курьера на слот.
//
//	booked -> started -> finished
//	booked -> cancelled
type BookingStatus string

const (
	StatusBooked    BookingStatus = "booked"
	StatusCancelled BookingStatus = "cancelled"
	StatusStarted   BookingStatus = "started"
	StatusFinished  BookingStatus = "finished"
)

type Courier struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Slot — интервал времени в зоне доставки, на который могут записаться несколько курьеров.
// Деньги хранятся целым числом копеек, чтобы не терять точность на float.
type Slot struct {
	ID                int64     `json:"id"`
	Zone              string    `json:"zone"`
	StartsAt          time.Time `json:"starts_at"`
	EndsAt            time.Time `json:"ends_at"`
	Capacity          int       `json:"capacity"`
	HourlyRateKopecks int64     `json:"hourly_rate_kopecks"`
	Booked            int       `json:"booked"`
	FreePlaces        int       `json:"free_places"`
	CreatedAt         time.Time `json:"created_at"`
}

type Booking struct {
	ID            int64         `json:"id"`
	SlotID        int64         `json:"slot_id"`
	CourierID     int64         `json:"courier_id"`
	Status        BookingStatus `json:"status"`
	StartedAt     *time.Time    `json:"started_at,omitempty"`
	FinishedAt    *time.Time    `json:"finished_at,omitempty"`
	EarnedKopecks *int64        `json:"earned_kopecks,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
}

// NewSlot — данные для создания слота.
type NewSlot struct {
	Zone              string
	StartsAt          time.Time
	EndsAt            time.Time
	Capacity          int
	HourlyRateKopecks int64
}

// SlotFilter — параметры выборки слотов.
type SlotFilter struct {
	Zone  string    // пустая строка — любая зона
	From  time.Time // слоты, начинающиеся не раньше From
	To    time.Time // нулевое значение — без верхней границы
	Limit int
}

// Repository — то, что сервису нужно от хранилища.
type Repository interface {
	CreateCourier(ctx context.Context, name string) (Courier, error)
	CreateSlot(ctx context.Context, s NewSlot) (Slot, error)
	ListSlots(ctx context.Context, f SlotFilter) ([]Slot, error)

	// Book атомарно проверяет свободные места и создаёт запись.
	// Может вернуть ErrNotFound, ErrSlotClosed, ErrSlotFull, ErrOverlap.
	Book(ctx context.Context, slotID, courierID int64, now time.Time) (Booking, error)

	GetBooking(ctx context.Context, id int64) (Booking, Slot, error)

	// Cancel, Start и Finish меняют статус только из допустимого исходного состояния
	// (одним UPDATE ... WHERE status = ...), поэтому гонки между запросами невозможны.
	// Если запись есть, но статус не подходит — ErrInvalidState, если записи нет — ErrNotFound.
	Cancel(ctx context.Context, id int64) (Booking, error)
	Start(ctx context.Context, id int64, at time.Time) (Booking, error)
	Finish(ctx context.Context, id int64, at time.Time, earnedKopecks int64) (Booking, error)

	ListCourierBookings(ctx context.Context, courierID int64) ([]Booking, error)
}
