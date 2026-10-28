package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mashna33/go-courier-shifts/internal/shifts"
)

// stubService подменяет бизнес-логику: HTTP-слой проверяем отдельно от базы данных.
type stubService struct {
	err   error // ошибка, которую вернут все методы
	panic bool  // вызвать панику внутри метода

	gotFilter    shifts.SlotFilter
	gotSlot      shifts.NewSlot
	gotSlotID    int64
	gotCourierID int64
}

var _ Service = (*stubService)(nil)

func (s *stubService) maybePanic() {
	if s.panic {
		panic("boom")
	}
}

func (s *stubService) CreateCourier(_ context.Context, name string) (shifts.Courier, error) {
	s.maybePanic()
	return shifts.Courier{ID: 7, Name: name}, s.err
}

func (s *stubService) CreateSlot(_ context.Context, in shifts.NewSlot) (shifts.Slot, error) {
	s.gotSlot = in
	return shifts.Slot{ID: 3, Zone: in.Zone}, s.err
}

func (s *stubService) ListSlots(_ context.Context, f shifts.SlotFilter) ([]shifts.Slot, error) {
	s.gotFilter = f
	return []shifts.Slot{}, s.err
}

func (s *stubService) Book(_ context.Context, slotID, courierID int64) (shifts.Booking, error) {
	s.gotSlotID, s.gotCourierID = slotID, courierID
	return shifts.Booking{ID: 11, SlotID: slotID, CourierID: courierID, Status: shifts.StatusBooked}, s.err
}

func (s *stubService) Cancel(context.Context, int64) (shifts.Booking, error) {
	return shifts.Booking{}, s.err
}

func (s *stubService) Start(context.Context, int64) (shifts.Booking, error) {
	return shifts.Booking{}, s.err
}

func (s *stubService) Finish(context.Context, int64) (shifts.Booking, error) {
	return shifts.Booking{}, s.err
}

func (s *stubService) CourierBookings(context.Context, int64) ([]shifts.Booking, error) {
	return []shifts.Booking{}, s.err
}

func newHandler(svc Service, ping func(context.Context) error) http.Handler {
	if ping == nil {
		ping = func(context.Context) error { return nil }
	}
	return New(svc, ping, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not an error JSON: %v (%s)", err, rec.Body.String())
	}
	return e
}

func TestHealthAndReadiness(t *testing.T) {
	h := newHandler(&stubService{}, nil)
	if rec := do(h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/readyz", ""); rec.Code != http.StatusOK {
		t.Errorf("/readyz = %d, want 200", rec.Code)
	}

	down := newHandler(&stubService{}, func(context.Context) error { return errors.New("db down") })
	rec := do(down, http.MethodGet, "/readyz", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz with broken db = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "db down") {
		t.Error("readiness response must not leak the internal error")
	}
}

func TestCreateCourier(t *testing.T) {
	h := newHandler(&stubService{}, nil)
	rec := do(h, http.MethodPost, "/couriers", `{"name":"Иван"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var c shifts.Courier
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil || c.ID != 7 || c.Name != "Иван" {
		t.Fatalf("unexpected body: %s (err %v)", rec.Body.String(), err)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestBadRequests(t *testing.T) {
	h := newHandler(&stubService{}, nil)

	tests := []struct {
		name         string
		method, path string
		body         string
	}{
		{"битый JSON", http.MethodPost, "/couriers", `{"name":`},
		{"неизвестное поле", http.MethodPost, "/couriers", `{"name":"a","admin":true}`},
		{"два объекта в теле", http.MethodPost, "/couriers", `{"name":"a"}{"name":"b"}`},
		{"пустое тело", http.MethodPost, "/couriers", ``},
		{"нечисловой id слота", http.MethodPost, "/slots/abc/bookings", `{"courier_id":1}`},
		{"нулевой id слота", http.MethodPost, "/slots/0/bookings", `{"courier_id":1}`},
		{"отсутствует courier_id", http.MethodPost, "/slots/1/bookings", `{}`},
		{"отрицательный courier_id", http.MethodPost, "/slots/1/bookings", `{"courier_id":-5}`},
		{"нечисловой id записи", http.MethodPost, "/bookings/x/cancel", ``},
		{"некорректная дата from", http.MethodGet, "/slots?from=вчера", ``},
		{"некорректная дата to", http.MethodGet, "/slots?to=2026-13-45", ``},
		{"нечисловой limit", http.MethodGet, "/slots?limit=много", ``},
		{"слот: дата не в формате RFC 3339", http.MethodPost, "/slots", `{"zone":"a","starts_at":"завтра"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := do(h, tt.method, tt.path, tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			e := decodeError(t, rec)
			if e.Code != "invalid_input" {
				t.Fatalf("code = %q, want invalid_input", e.Code)
			}
			if strings.Contains(e.Error, "\n") || !strings.HasPrefix(e.Error, "invalid input: ") {
				t.Fatalf("error message must be one line starting with 'invalid input: ', got %q", e.Error)
			}
		})
	}
}

func TestBodyTooLarge(t *testing.T) {
	h := newHandler(&stubService{}, nil)
	body := `{"name":"` + strings.Repeat("a", maxBodyBytes+10) + `"}`
	if rec := do(h, http.MethodPost, "/couriers", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDomainErrorsAreMappedToStatuses(t *testing.T) {
	tests := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{shifts.ErrInvalidInput, http.StatusBadRequest, "invalid_input"},
		{shifts.ErrNotFound, http.StatusNotFound, "not_found"},
		{shifts.ErrSlotFull, http.StatusConflict, "slot_full"},
		{shifts.ErrSlotClosed, http.StatusConflict, "slot_closed"},
		{shifts.ErrOverlap, http.StatusConflict, "shift_overlap"},
		{shifts.ErrInvalidState, http.StatusConflict, "invalid_state"},
		{shifts.ErrCancelTooLate, http.StatusConflict, "cancel_too_late"},
		{shifts.ErrOutsideStartWindow, http.StatusConflict, "outside_start_window"},
		// Ошибка, обёрнутая через fmt.Errorf("%w"), тоже должна распознаваться.
		{errors.Join(errors.New("context"), shifts.ErrSlotFull), http.StatusConflict, "slot_full"},
	}
	for _, tt := range tests {
		t.Run(tt.wantCode, func(t *testing.T) {
			h := newHandler(&stubService{err: tt.err}, nil)
			rec := do(h, http.MethodPost, "/bookings/1/cancel", "")
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if e := decodeError(t, rec); e.Code != tt.wantCode {
				t.Fatalf("code = %q, want %q", e.Code, tt.wantCode)
			}
		})
	}
}

func TestUnknownErrorDoesNotLeak(t *testing.T) {
	h := newHandler(&stubService{err: errors.New("pq: password authentication failed for user shifts")}, nil)
	rec := do(h, http.MethodPost, "/bookings/1/start", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("internal error leaked to the client: %s", rec.Body.String())
	}
	if e := decodeError(t, rec); e.Code != "internal" {
		t.Fatalf("code = %q, want internal", e.Code)
	}
}

func TestPanicIsRecovered(t *testing.T) {
	h := newHandler(&stubService{panic: true}, nil)
	rec := do(h, http.MethodPost, "/couriers", `{"name":"a"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// Сервер продолжает работать после паники.
	if rec := do(h, http.MethodGet, "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("server is not usable after panic: %d", rec.Code)
	}
}

func TestBookPassesIDs(t *testing.T) {
	svc := &stubService{}
	rec := do(newHandler(svc, nil), http.MethodPost, "/slots/42/bookings", `{"courier_id":9}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if svc.gotSlotID != 42 || svc.gotCourierID != 9 {
		t.Fatalf("service got slot=%d courier=%d, want 42 and 9", svc.gotSlotID, svc.gotCourierID)
	}
}

func TestListSlotsParsesQuery(t *testing.T) {
	svc := &stubService{}
	rec := do(newHandler(svc, nil), http.MethodGet,
		"/slots?zone=center&from=2026-11-01T10:00:00Z&to=2026-11-02T10:00:00Z&limit=5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	want := shifts.SlotFilter{
		Zone:  "center",
		From:  time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
		To:    time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC),
		Limit: 5,
	}
	if svc.gotFilter.Zone != want.Zone || !svc.gotFilter.From.Equal(want.From) ||
		!svc.gotFilter.To.Equal(want.To) || svc.gotFilter.Limit != want.Limit {
		t.Fatalf("filter = %+v, want %+v", svc.gotFilter, want)
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("empty list must be encoded as [], got %s", rec.Body.String())
	}
}

func TestCreateSlotParsesBody(t *testing.T) {
	svc := &stubService{}
	body := `{"zone":"center","starts_at":"2026-11-01T10:00:00+07:00","ends_at":"2026-11-01T14:00:00+07:00","capacity":3,"hourly_rate_kopecks":30000}`
	rec := do(newHandler(svc, nil), http.MethodPost, "/slots", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	if svc.gotSlot.Zone != "center" || svc.gotSlot.Capacity != 3 || svc.gotSlot.HourlyRateKopecks != 30000 {
		t.Fatalf("unexpected slot: %+v", svc.gotSlot)
	}
	if d := svc.gotSlot.EndsAt.Sub(svc.gotSlot.StartsAt); d != 4*time.Hour {
		t.Fatalf("slot duration = %v, want 4h", d)
	}
}

func TestUnknownRouteAndWrongMethod(t *testing.T) {
	h := newHandler(&stubService{}, nil)
	if rec := do(h, http.MethodGet, "/nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown route = %d, want 404", rec.Code)
	}
	if rec := do(h, http.MethodDelete, "/slots", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method = %d, want 405", rec.Code)
	}
}
