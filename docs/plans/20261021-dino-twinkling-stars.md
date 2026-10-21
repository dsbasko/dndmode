# Мерцающие звёзды в ночной теме `dino`

## Overview
- Сейчас ночью на небе две статичные звезды-крестика. Делаем шесть, и каждая мерцает в своём ритме: «+» → «×» → «·» → «×» → «+».
- Звёзды не мигают в такт: у каждой свой период (2–4 с) и сдвиг фазы.
- Остальное без изменений: дрейф, полоса неба, растворение днём через палитру, нового C нет.

## Context (from discovery)
- Код: `internal/linux/overlay/dino.go` (`dinoRun.stars []dinoCloud`, `dinoStars = 2`, `dinoStarW`, `init`, `move`, `frame`), `dino_art.go` (`dinoArtStar` — один кадр, до земли), `dino_test.go` (`TestDinoArt_IsWellFormed`, `TestDinoSky_StartsOnScreen`, `TestDinoSky_DriftsAndWraps`, `TestDinoView_FrameBlits`).
- Генератор неба — `cloudRng`; `skyLevel()` даёт высоту в полосе 10..60.
- Кадр арта считается от `r.steps` без состояния — так уже сделаны ноги динозаврика и крылья птицы.
- Упоминания «two stars» / «moon and stars»: `dino.go:13`, `dino.go:85`, `config.go:124`, `config.go:1025`, `README.md:576`, `README.md:971`, раздел `dino` в README.
- Предыдущие планы: `docs/plans/completed/20261021-dino-overlay-style.md`, `docs/plans/completed/20261021-dino-day-night.md`.

## Development Approach
- **testing approach**: TDD — тесты первыми.
- каждую задачу доводить до конца перед следующей; маленькие правки.
- **CRITICAL: каждая задача включает новые/обновлённые тесты.**
- **CRITICAL: все тесты проходят перед следующей задачей.**
- **CRITICAL: при изменении объёма обновлять этот план.**
- тест «автопилот никогда не касается» не трогаем, он должен оставаться зелёным.

## Testing Strategy
- **unit tests**: `go test -race ./internal/linux/overlay` на каждой задаче.
- **превью**: временный manual-тест рисует несколько кадров неба в PNG; после проверки удаляется.
- **e2e UI-тестов в проекте нет**; живой прогон — в Post-Completion.

## Progress Tracking
- отмечать `[x]` сразу по выполнении
- новые задачи — с ➕, блокеры — с ⚠️
- держать план в синхроне с работой

## Solution Overview
- Звезда — свой тип `dinoStar{x, y, period, phase}`; кадр — чистая функция `starFrame(steps)` от общего счётчика шагов, как у ног и крыльев. Состояния мерцания нет, поэтому тесты точные.
- Три кадра арта 3×3 вместо одного; индексы звезды остаются до земли, цифры — последними, поэтому проверки кадра по диапазону цифр не ломаются.

## Technical Details
- **Арт** (`dino_art.go`), символ `s`:

  | Кадр | Строки | Смысл |
  |---|---|---|
  | `star0` | `.s.` `sss` `.s.` | «+», горит |
  | `star1` | `s.s` `.s.` `s.s` | «×», блик повернулся |
  | `star2` | `...` `.s.` `...` | «·», почти погасла |

  `dinoArtStar` — 3 кадра подряд; `dinoArtGround = dinoArtStar + 3`.
- **Модель** (`dino.go`):
  - `dinoStars = 6`; `dinoStarPeriodMin = 120`, `dinoStarPeriodMax = 240` шагов.
  - `type dinoStar struct { x float64; y, period, phase int }`; `dinoRun.stars []dinoStar`; `dinoCloud` снова только для облаков.
  - `newStar(x)` берёт `y = skyLevel()`, `period` в `[120, 240]`, `phase` в `[0, period)` из `cloudRng`; вызывается в `init` (x случайно в `[0, w − dinoStarW)`) и при появлении справа (`x = w + dinoSpawnAt`).
  - `func (s *dinoStar) frame(steps int64) int`: `t = (steps + phase) % period`; `t < 2·period/3` → «+»; оставшаяся треть делится на 4 равные части: «×», «·», «×», «+». Возвращает `dinoArtStar + 0/1/2`.
- **Отрисовка**: `frame()` блитит `s.frame(r.steps)` вместо `dinoArtStar`.
- Дрейф `dinoSkyDrift`, полоса `dinoSkyTop..dinoSkyBottom`, палитра и днёвное растворение — без изменений.

## What Goes Where
- **Implementation Steps**: код, тесты, README и шаблон конфига.
- **Post-Completion**: живой прогон, переустановка бинарника, коммит и пуш — по запросу пользователя.

## Implementation Steps

### Task 1: Кадры звезды в арте

**Files:**
- Modify: `internal/linux/overlay/dino_art.go`
- Modify: `internal/linux/overlay/dino_test.go`

- [ ] обновить `TestDinoArt_IsWellFormed`: `{"star", 3, 3, 3}` в списке `want` (после луны, до земли)
- [ ] нарисовать `star0`/`star1`/`star2`, сдвинуть `dinoArtGround` на `dinoArtStar + 3`
- [ ] прогнать `go test -race ./internal/linux/overlay` — должно пройти (кадр звезды пока всегда `star0`)

### Task 2: Шесть звёзд со своим ритмом

**Files:**
- Modify: `internal/linux/overlay/dino.go`
- Modify: `internal/linux/overlay/dino_test.go`

- [ ] перенести `TestDinoSky_StartsOnScreen` и `TestDinoSky_DriftsAndWraps` на `dinoStar` и `dinoStars = 6`
- [ ] написать тест: у каждой звезды после `init` `period` в `[120, 240]`, `phase` в `[0, period)`; не все шесть с одинаковой парой `period/phase`
- [ ] написать тест: звезда, ушедшая за левый край, появляется справа с новой высотой и новыми `period/phase` в диапазонах
- [ ] ввести `dinoStar`, `newStar`, `dinoStars = 6`; вернуть doc-комментарий `dinoCloud` к облакам
- [ ] прогнать тесты — должно пройти

### Task 3: Цикл мерцания и отрисовка

**Files:**
- Modify: `internal/linux/overlay/dino.go`
- Modify: `internal/linux/overlay/dino_test.go`

- [ ] написать тест цикла: за один период кадры идут строго «+» → «×» → «·» → «×» → «+» (сжатая последовательность), «+» занимает первые `2·period/3` шагов; для нескольких `period/phase`, включая не кратные 12
- [ ] обновить `TestDinoView_FrameBlits`: кадры звёзд в диапазоне `[dinoArtStar, dinoArtStar+3)`, по-прежнему раньше облаков
- [ ] реализовать `dinoStar.frame(steps)` и рисовать его в `frame()`
- [ ] прогнать тесты — должно пройти

### Task 4: Проверка критериев приёмки
- [ ] все требования из Overview реализованы
- [ ] `make test`
- [ ] `make lint` — без новых замечаний (4 старых вне задачи остаются)
- [ ] `make build-linux`
- [ ] превью нескольких кадров неба в PNG временным manual-тестом, затем удалить его

### Task 5: [Final] Документация
- [ ] `dino.go`: комментарии про «two stars» (описание стиля, блок констант неба)
- [ ] `internal/config/config.go`: doc-комментарий `OverlayStyleDino` и шаблон конфига — «a moon and twinkling stars»
- [ ] README: копия шаблона, строка `dino` в таблице стилей, раздел `dino`
- [ ] перенести план в `docs/plans/completed/`

## Post-Completion
*Ручные шаги, без чекбоксов*

**Ручная проверка:**
- `./dndmode --style dino --timer 1m`: шесть звёзд ночью мерцают вразнобой, днём их не видно

**По запросу пользователя:**
- переустановить бинарник и перезапустить `--watch`; коммит и пуш в форк
