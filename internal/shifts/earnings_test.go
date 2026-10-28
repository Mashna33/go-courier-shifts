package shifts

import (
	"testing"
	"time"
)

func TestCalcEarnings(t *testing.T) {
	slotStart := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	slotEnd := slotStart.Add(4 * time.Hour)
	const rate = 30000 // 300 рублей в час, в копейках

	tests := []struct {
		name     string
		started  time.Time
		finished time.Time
		want     int64
	}{
		{
			name:     "вся смена целиком",
			started:  slotStart,
			finished: slotEnd,
			want:     4 * rate,
		},
		{
			name:     "начал с опозданием: оплачивается только отработанное время",
			started:  slotStart.Add(30 * time.Minute),
			finished: slotEnd,
			want:     rate * 7 / 2, // 3,5 часа
		},
		{
			name:     "начал раньше слота: время до начала слота не оплачивается",
			started:  slotStart.Add(-15 * time.Minute),
			finished: slotStart.Add(time.Hour),
			want:     rate,
		},
		{
			name:     "закончил позже конца слота: лишнее время не оплачивается",
			started:  slotStart,
			finished: slotEnd.Add(2 * time.Hour),
			want:     4 * rate,
		},
		{
			name:     "завершил сразу после старта",
			started:  slotStart,
			finished: slotStart,
			want:     0,
		},
		{
			name:     "завершил раньше, чем начал (сбой часов)",
			started:  slotStart.Add(time.Hour),
			finished: slotStart,
			want:     0,
		},
		{
			name:     "смена целиком вне слота",
			started:  slotEnd.Add(time.Minute),
			finished: slotEnd.Add(time.Hour),
			want:     0,
		},
		{
			name:     "дробные секунды отбрасываются, копейки округляются вниз",
			started:  slotStart,
			finished: slotStart.Add(1500 * time.Millisecond),
			want:     rate * 1 / 3600, // 1 секунда -> 8 копеек (30000/3600 = 8,33)
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalcEarnings(rate, slotStart, slotEnd, tt.started, tt.finished)
			if got != tt.want {
				t.Fatalf("CalcEarnings() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCalcEarningsZeroRate(t *testing.T) {
	start := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if got := CalcEarnings(0, start, start.Add(time.Hour), start, start.Add(time.Hour)); got != 0 {
		t.Fatalf("zero rate must give zero earnings, got %d", got)
	}
}
