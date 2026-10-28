package shifts

import "time"

// CalcEarnings считает заработок за смену в копейках.
//
// Оплачивается только время внутри слота: если курьер начал смену раньше начала слота
// или закончил позже его конца, лишнее время не учитывается. Если отработанное время
// отрицательно или нулевое, заработок равен нулю.
// Считаем в целых секундах, чтобы результат не зависел от погрешностей float.
func CalcEarnings(hourlyRateKopecks int64, slotStart, slotEnd, startedAt, finishedAt time.Time) int64 {
	begin := startedAt
	if begin.Before(slotStart) {
		begin = slotStart
	}
	end := finishedAt
	if end.After(slotEnd) {
		end = slotEnd
	}
	if !end.After(begin) {
		return 0
	}
	seconds := int64(end.Sub(begin) / time.Second)
	return hourlyRateKopecks * seconds / 3600
}
