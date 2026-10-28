// Package api — HTTP-слой: разбор запросов, вызов сервиса и преобразование ошибок в ответы.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/Mashna33/go-courier-shifts/internal/shifts"
)

const maxBodyBytes = 1 << 20

// Service — то, что HTTP-слою нужно от бизнес-логики. Интерфейс объявлен здесь,
// у потребителя, поэтому в тестах его легко подменить заглушкой.
type Service interface {
	CreateCourier(ctx context.Context, name string) (shifts.Courier, error)
	CreateSlot(ctx context.Context, in shifts.NewSlot) (shifts.Slot, error)
	ListSlots(ctx context.Context, f shifts.SlotFilter) ([]shifts.Slot, error)
	Book(ctx context.Context, slotID, courierID int64) (shifts.Booking, error)
	Cancel(ctx context.Context, bookingID int64) (shifts.Booking, error)
	Start(ctx context.Context, bookingID int64) (shifts.Booking, error)
	Finish(ctx context.Context, bookingID int64) (shifts.Booking, error)
	CourierBookings(ctx context.Context, courierID int64) ([]shifts.Booking, error)
}

type server struct {
	svc  Service
	ping func(context.Context) error
	log  *slog.Logger
}

// New собирает маршруты. ping используется в /readyz для проверки доступности базы.
func New(svc Service, ping func(context.Context) error, log *slog.Logger) http.Handler {
	s := &server{svc: svc, ping: ping, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)

	mux.HandleFunc("POST /couriers", s.createCourier)
	mux.HandleFunc("GET /couriers/{id}/bookings", s.courierBookings)

	mux.HandleFunc("POST /slots", s.createSlot)
	mux.HandleFunc("GET /slots", s.listSlots)
	mux.HandleFunc("POST /slots/{id}/bookings", s.book)

	mux.HandleFunc("POST /bookings/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /bookings/{id}/start", s.start)
	mux.HandleFunc("POST /bookings/{id}/finish", s.finish)

	return recoverer(log, requestLogger(log, mux))
}

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.ping(ctx); err != nil {
		s.log.Warn("readiness check failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type createCourierRequest struct {
	Name string `json:"name"`
}

func (s *server) createCourier(w http.ResponseWriter, r *http.Request) {
	var req createCourierRequest
	if !s.decode(w, r, &req) {
		return
	}
	c, err := s.svc.CreateCourier(r.Context(), req.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *server) courierBookings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	list, err := s.svc.CourierBookings(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type createSlotRequest struct {
	Zone              string    `json:"zone"`
	StartsAt          time.Time `json:"starts_at"`
	EndsAt            time.Time `json:"ends_at"`
	Capacity          int       `json:"capacity"`
	HourlyRateKopecks int64     `json:"hourly_rate_kopecks"`
}

func (s *server) createSlot(w http.ResponseWriter, r *http.Request) {
	var req createSlotRequest
	if !s.decode(w, r, &req) {
		return
	}
	slot, err := s.svc.CreateSlot(r.Context(), shifts.NewSlot{
		Zone:              req.Zone,
		StartsAt:          req.StartsAt,
		EndsAt:            req.EndsAt,
		Capacity:          req.Capacity,
		HourlyRateKopecks: req.HourlyRateKopecks,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, slot)
}

func (s *server) listSlots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := shifts.SlotFilter{Zone: q.Get("zone")}

	var err error
	if f.From, err = parseTimeParam(q.Get("from")); err != nil {
		s.fail(w, r, invalidParam("from", "must be an RFC 3339 timestamp"))
		return
	}
	if f.To, err = parseTimeParam(q.Get("to")); err != nil {
		s.fail(w, r, invalidParam("to", "must be an RFC 3339 timestamp"))
		return
	}
	if v := q.Get("limit"); v != "" {
		if f.Limit, err = strconv.Atoi(v); err != nil {
			s.fail(w, r, invalidParam("limit", "must be an integer"))
			return
		}
	}

	slots, err := s.svc.ListSlots(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, slots)
}

type bookRequest struct {
	CourierID int64 `json:"courier_id"`
}

func (s *server) book(w http.ResponseWriter, r *http.Request) {
	slotID, ok := s.pathID(w, r)
	if !ok {
		return
	}
	var req bookRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.CourierID <= 0 {
		s.fail(w, r, invalidParam("courier_id", "must be a positive integer"))
		return
	}
	b, err := s.svc.Book(r.Context(), slotID, req.CourierID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *server) cancel(w http.ResponseWriter, r *http.Request) { s.transition(w, r, s.svc.Cancel) }
func (s *server) start(w http.ResponseWriter, r *http.Request)  { s.transition(w, r, s.svc.Start) }
func (s *server) finish(w http.ResponseWriter, r *http.Request) { s.transition(w, r, s.svc.Finish) }

func (s *server) transition(w http.ResponseWriter, r *http.Request,
	fn func(context.Context, int64) (shifts.Booking, error)) {
	id, ok := s.pathID(w, r)
	if !ok {
		return
	}
	b, err := fn(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// --- вспомогательные функции ---

// invalidParam формирует ошибку валидации с одной строкой сообщения в том же формате,
// что и ошибки бизнес-логики: "invalid input: <что не так>".
func invalidParam(name, msg string) error {
	return fmt.Errorf("%w: %s %s", shifts.ErrInvalidInput, name, msg)
}

func parseTimeParam(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, v)
}

func (s *server) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.fail(w, r, invalidParam("id", "must be a positive integer"))
		return 0, false
	}
	return id, true
}

// decode читает JSON-тело: ограничивает размер, запрещает неизвестные поля и лишние данные после объекта.
func (s *server) decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		s.fail(w, r, invalidParam("body", "is not valid JSON for this endpoint: "+err.Error()))
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		s.fail(w, r, invalidParam("body", "must contain a single JSON object"))
		return false
	}
	return true
}

var errorTable = []struct {
	err    error
	status int
	code   string
}{
	{shifts.ErrInvalidInput, http.StatusBadRequest, "invalid_input"},
	{shifts.ErrNotFound, http.StatusNotFound, "not_found"},
	{shifts.ErrSlotFull, http.StatusConflict, "slot_full"},
	{shifts.ErrSlotClosed, http.StatusConflict, "slot_closed"},
	{shifts.ErrOverlap, http.StatusConflict, "shift_overlap"},
	{shifts.ErrInvalidState, http.StatusConflict, "invalid_state"},
	{shifts.ErrCancelTooLate, http.StatusConflict, "cancel_too_late"},
	{shifts.ErrOutsideStartWindow, http.StatusConflict, "outside_start_window"},
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// fail превращает доменную ошибку в ответ. Неизвестные ошибки логируем,
// а клиенту отдаём общее сообщение, чтобы не раскрывать внутренности.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	for _, e := range errorTable {
		if errors.Is(err, e.err) {
			writeJSON(w, e.status, errorBody{Error: err.Error(), Code: e.code})
			return
		}
	}
	s.log.Error("internal error", "method", r.Method, "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal server error", Code: "internal"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
