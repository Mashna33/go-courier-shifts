#!/usr/bin/env bash
# Сквозная проверка запущенного сервиса: проходит всю жизнь смены через HTTP.
# Использование: scripts/smoke.sh [адрес]   (по умолчанию http://localhost:8080)
set -euo pipefail

BASE="${1:-http://localhost:8080}"

fail() { echo "FAIL: $*" >&2; exit 1; }

# call МЕТОД ПУТЬ [ТЕЛО] -> печатает тело, код ответа кладёт в $STATUS
call() {
  local method=$1 path=$2 body=${3:-}
  local out
  if [ -n "$body" ]; then
    out=$(curl -sS -X "$method" -H 'Content-Type: application/json' -d "$body" -w '\n%{http_code}' "$BASE$path")
  else
    out=$(curl -sS -X "$method" -w '\n%{http_code}' "$BASE$path")
  fi
  STATUS=${out##*$'\n'}
  BODY=${out%$'\n'*}
}

expect() { # expect КОД ОПИСАНИЕ
  [ "$STATUS" = "$1" ] || fail "$2: ожидали HTTP $1, получили $STATUS ($BODY)"
  echo "ok: $2"
}

echo "Ждём готовности сервиса на $BASE ..."
for _ in $(seq 1 30); do
  if curl -fsS "$BASE/readyz" >/dev/null 2>&1; then break; fi
  sleep 1
done
call GET /readyz; expect 200 "сервис и база готовы"

# Слот начинается через 10 минут: смену уже можно начать (окно — за 15 минут до старта).
starts=$(date -u -d '+10 minutes' +%Y-%m-%dT%H:%M:%SZ)
ends=$(date -u -d '+130 minutes' +%Y-%m-%dT%H:%M:%SZ)

call POST /couriers '{"name":"Иван"}'; expect 201 "курьер создан"
courier=$(jq -r .id <<<"$BODY")

call POST /slots "{\"zone\":\"smoke\",\"starts_at\":\"$starts\",\"ends_at\":\"$ends\",\"capacity\":1,\"hourly_rate_kopecks\":30000}"
expect 201 "слот создан"
slot=$(jq -r .id <<<"$BODY")

call GET "/slots?zone=smoke"; expect 200 "список слотов"
[ "$(jq -r '.[0].free_places' <<<"$BODY")" = "1" ] || fail "в новом слоте должно быть 1 свободное место"

call POST "/slots/$slot/bookings" "{\"courier_id\":$courier}"; expect 201 "курьер записан на слот"
booking=$(jq -r .id <<<"$BODY")

call POST /couriers '{"name":"Пётр"}'; expect 201 "второй курьер создан"
other=$(jq -r .id <<<"$BODY")
call POST "/slots/$slot/bookings" "{\"courier_id\":$other}"; expect 409 "в заполненный слот записаться нельзя"
[ "$(jq -r .code <<<"$BODY")" = "slot_full" ] || fail "ожидали код slot_full, получили $BODY"

call POST "/bookings/$booking/start"; expect 200 "смена начата"
call POST "/bookings/$booking/finish"; expect 200 "смена завершена"
[ "$(jq -r .status <<<"$BODY")" = "finished" ] || fail "ожидали статус finished, получили $BODY"

call POST "/bookings/$booking/finish"; expect 409 "повторное завершение отклонено"
call GET "/couriers/$courier/bookings"; expect 200 "история смен курьера"
call GET "/couriers/999999/bookings"; expect 404 "неизвестный курьер даёт 404"

echo "Все проверки пройдены."
