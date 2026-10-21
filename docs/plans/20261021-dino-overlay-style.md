# Стиль щита `dino` (Linux)

## Overview
- Новый стиль экрана блокировки `dino` в духе Chrome T-Rex runner, только для Linux.
- Динозаврик бежит сам (автопилот): перепрыгивает кактусы и низких птеродактилей, пригибается под средними, проходит под высокими. Забег бесконечный, без ошибок и Game Over.
- Ввод по-прежнему полностью перехвачен: стиль чисто косметический, щит непрозрачный, все гарантии блокировки — как у `black`.
- macOS принимает `dino` в конфиге (общая валидация), но отказывает при запуске с exit 1 — зеркально тому, как Linux отказывает `glass`.

## Context (from discovery)
- Модель стилей Linux: `internal/linux/overlay/{dvd,matrix,terminal,gold,bamboo}.go` — чистый Go; клей cgo — `anim_linux.go` (`animation`, `stillFrame`, `newAnimation`); список стилей — `Styles` в `overlay_linux.go`.
- Образец: `bamboo_art.go` (палитра-строка, `.` прозрачный), `bambooView` (`unit`, `frameSize`, `tick` с аккумулятором `bambooFPS`, `frame`), `bambooAnim` (`tick/still/upload/draw/free`).
- Переиспользуемые типы: `bambooImage`, `bambooARGB`, `bambooBlit` (`bamboo.go`). `bambooBake` привязан к палитре и туману bamboo — для dino не подходит.
- C-примитивы: `overlay_sprites_new/free`, `overlay_canvas_new/free/paint/show`. Холст — `CAIRO_FORMAT_RGB24` и **не очищается** между кадрами: первым блитом должен идти непрозрачный фон.
- RNG: `goldRand`/`goldUniform`/`goldSeed` (`gold.go`); сид анимации — `rand.Uint64()`, как в bamboo.
- Валидация: `config.ValidateOverlayStyle` (общая), затем отсев по `overlay.Styles` в `main_linux.go`; в darwin `main.go` отсева нет.
- Тесты: `internal/config/config_test.go` (константы, валидация, шаблон); `cmd/dndmode/*_test.go` — почти все `//go:build darwin`, на Linux не запускаются; smoke `overlay_smoketest_test.go` (тег `manual`) уже перебирает все `Styles`.

## Development Approach
- **testing approach**: TDD — сначала тесты (инварианты автопилота, физика, зазоры), затем код.
- каждую задачу доводить до конца перед следующей; маленькие сфокусированные правки.
- **CRITICAL: каждая задача включает новые/обновлённые тесты.**
- **CRITICAL: все тесты проходят перед следующей задачей.**
- **CRITICAL: при изменении объёма обновлять этот план.**
- darwin-пакеты здесь не собираются: правки darwin-пути проверяются чтением кода и на маке (Post-Completion).

## Testing Strategy
- **unit tests**: `go test -race ./internal/linux/overlay ./internal/config` на каждой задаче; модель `dino` тестируется без cgo/GUI.
- **smoke**: `go test -tags=manual -run Smoke ./internal/linux/overlay` — нужна десктопная сессия; `dino` попадает туда через `Styles`.
- **e2e UI-тестов в проекте нет**; ручной прогон — в Post-Completion.

## Progress Tracking
- отмечать `[x]` сразу по выполнении
- новые задачи — с префиксом ➕, блокеры — с ⚠️
- держать план в синхроне с фактической работой

## Solution Overview
- **Отрисовка — конвейер bamboo**: спрайты из `dino_art.go` запекаются один раз в ARGB, кадр рисуется в офскрин-холст в арт-разрешении (`overlay_canvas_paint`), выводится увеличенным nearest-neighbour (`overlay_canvas_show`). Перерисовка холста только при изменении кадра (`still()`).
- **Холст размером с полосу**, а не с экран: облака внутри полосы, как в оригинале; полоса выводится по центру через `overlay_canvas_show(cr, canvas, 0, yOffset, unit)`, остальное виджет уже залил чёрным. Меньше пикселей на кадр при 60 Гц.
- **Свой пиксель-арт** в духе оригинала: нет лицензионных вопросов и бинарных ресурсов.
- **Ночная тема**: `#ACACAC` — динозаврик, препятствия, земля, счёт; `#5F5F5F` — облака и камешки; фон — непрозрачный чёрный блит.
- **Без новых ключей конфига, ресурсов и C-кода.** Без `HI` (при бесконечном забеге он равен текущему счёту).
- `dinoAnim` — копия `bambooAnim` (подход «стиль на файл»); общий `spriteCanvas` не выносим — это рефакторинг bamboo вне объёма.

## Technical Details
- **Мир**: полоса 150 арт-пикселей; `unit = max(2, round(h/300))`; холст `ceil(w/unit) × 150`; `yOffset = (h − 150·unit)/2`.
- **Время**: `const dinoFPS = 60`, аккумулятор как в bamboo: `acc += min(max(dt,0), animMaxDt) * dinoFPS`; позиции округляются до арт-пикселя при построении кадра.
- **Физика** (константы Chrome): скорость 6 → 13 px/шаг, ускорение 0.001/шаг; прыжок `v0 = -10`, гравитация 0.6; пригибание меняет кадры и хитбокс.
- **Препятствия**: группы кактусов 1–3 (малые/большие), птеродактиль при скорости > 8.5 на трёх высотах, движется со скоростью мира (без собственного смещения — проще автопилот).
- **Минимальный зазор**: `airtime(v0, g) · speed + obstacleW + dinoW + margin`; фактический = `max(минимальный, ширина × скорость × коэффициент + случай)`.
- **Автопилот**: кактус/низкая птица — прыжок на дистанции `speed × K`, средняя — пригнуться, высокая — бежать.
- **Счёт**: `distance × 0.025`, 5 цифр в правом верхнем углу полосы, по модулю 100000 (на скорости 13 переполнение через ~85 мин — щит в `--watch` висит часами); мигание (alpha) на каждой сотне. Сброс сам собой: `newAnimation` создаёт новую анимацию на каждое окно при подъёме щита.
- **Структуры**: `dinoRun` (мир, скорость, состояние динозаврика, очередь препятствий, облака, земля, счёт), `dinoView` (`dinoRun` + аккумулятор, `changed`, размеры), `dinoSheet` (индексы спрайтов), `dinoBake(grid, w, h) bambooImage`; кадр — `[]bambooBlit`.

## What Goes Where
- **Implementation Steps**: код, тесты, README в этом репозитории.
- **Post-Completion**: ручной прогон на десктопе, проверка darwin на маке.

## Implementation Steps

### Task 1: Пиксель-арт, палитра, запекание

**Files:**
- Create: `internal/linux/overlay/dino_art.go`
- Create: `internal/linux/overlay/dino_test.go`

- [ ] написать тесты валидности спрайтов: ровные строки, символы только из палитры, ожидаемые размеры каждого кадра
- [ ] написать тест `dinoBake`: `.` → прозрачный пиксель, символ палитры → цвет через `bambooARGB`
- [ ] создать `dino_art.go`: палитра (`#ACACAC`, `#5F5F5F`), динозаврик (2 кадра бега, прыжок, 2 кадра пригибания), кактусы 3 малых + 3 больших, птеродактиль 2 кадра, облако, 3 куска земли, цифры 0–9
- [ ] задать хитбоксы спрайтов (несколько прямоугольников на спрайт, как в Chrome) и тест: хитбоксы внутри границ спрайта
- [ ] реализовать `dinoBake(grid, w, h) bambooImage` со своей палитрой
- [ ] прогнать `go test -race ./internal/linux/overlay` — должно пройти

### Task 2: Физика забега и счёт

**Files:**
- Create: `internal/linux/overlay/dino.go`
- Modify: `internal/linux/overlay/dino_test.go`

- [ ] написать тесты скорости: растёт монотонно, не превышает 13, выходит на потолок за ~2 мин игрового времени
- [ ] написать тесты прыжка: старт с `v0 = -10`, гравитация 0.6, приземление на землю, ожидаемая высота и `airtime`
- [ ] написать тесты счёта: `distance × 0.025`, 5 цифр, переполнение по модулю 100000 (distance на 99999 и дальше), мигание на каждой сотне
- [ ] реализовать `dinoRun.init(seed, w)`, `step()` (скорость, прыжок, пригибание, дистанция, счёт)
- [ ] прогнать тесты — должно пройти

### Task 3: Генерация препятствий

**Files:**
- Modify: `internal/linux/overlay/dino.go`
- Modify: `internal/linux/overlay/dino_test.go`

- [ ] написать тесты: каждый зазор ≥ `airtime · speed + obstacleW + dinoW + margin` на текущей скорости (несколько сидов)
- [ ] написать тесты: птеродактиль не появляется при скорости ≤ 8.5; появляется на одной из трёх высот
- [ ] написать тест детерминизма: одинаковый сид → одинаковая последовательность препятствий
- [ ] реализовать очередь препятствий: спавн за правым краем, удаление за левым, RNG через `goldUniform`/`goldSeed`
- [ ] прогнать тесты — должно пройти

### Task 4: Автопилот

**Files:**
- Modify: `internal/linux/overlay/dino.go`
- Modify: `internal/linux/overlay/dino_test.go`

- [ ] написать главный тест: 10 сидов × 30 мин игрового времени (`t.Parallel()` по сидам), проверка на каждом шаге — ни одного пересечения хитбоксов динозаврика и препятствий
- [ ] написать тесты решений: кактус/низкая птица → прыжок, средняя → пригнуться, высокая → бег
- [ ] реализовать выбор действия по ближайшему препятствию и дистанции `speed × K`
- [ ] подобрать K и при необходимости `margin` зазора, пока главный тест не станет зелёным
- [ ] прогнать тесты — должно пройти

### Task 5: Облака, земля, кадр и шаг по времени

**Files:**
- Modify: `internal/linux/overlay/dino.go`
- Modify: `internal/linux/overlay/dino_test.go`

- [ ] написать тесты параллакса: земля движется со скоростью мира, облака медленнее
- [ ] написать тесты `frame()`: `blits[0]` — непрозрачный чёрный фон на весь холст (alpha 255); все блиты в границах холста; счёт в правом верхнем углу; мигание через alpha
- [ ] написать тесты `dinoView.tick`: первый вызов и смена размера пересидируют; `dt` меньше шага не меняет кадр (`changed == false`); `dt > animMaxDt` ограничивается; джиттер `dt = 1/59.94` и `1/60.05` даёт ровно один шаг на кадр в устойчивом режиме (без кадров 0/2 шага подряд)
- [ ] реализовать облака и куски земли, `dinoSheet`, `frame() []bambooBlit`
- [ ] реализовать `dinoView` (`unit`, `frameSize`, `yOffset`, `tick` с аккумулятором `dinoFPS`, `changed`, сид `rand.Uint64()`)
- [ ] прогнать тесты — должно пройти

### Task 6: Анимация в GTK-оверлее

**Files:**
- Modify: `internal/linux/overlay/anim_linux.go`
- Modify: `internal/linux/overlay/overlay_linux.go`
- Modify: `internal/linux/overlay/dino_test.go`

- [ ] написать тест: `newAnimation("dino", "", nil)` возвращает `*dinoAnim`; `slices.Contains(Styles, "dino")`
- [ ] добавить `dinoAnim` по образцу `bambooAnim`: `tick`, `still`, `upload`, `draw` (вывод полосы со сдвигом `yOffset`), `freeC`, `free`
- [ ] добавить `case "dino"` в `newAnimation`, `"dino"` в `Styles` и в комментарий пакета
- [ ] smoke покрывается существующим перебором `Styles` в `overlay_smoketest_test.go` — отдельный не нужен
- [ ] прогнать `go test -race ./internal/linux/overlay` и `go vet ./...` — должно пройти

### Task 7: Конфиг

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] расширить тесты: `OverlayStyleDino == "dino"`, `ValidateOverlayStyle("dino") == nil`, строка `overlay_style: dino` в таблице парсинга, `dino` в цикле проверки шаблона конфига
- [ ] добавить `OverlayStyleDino` с doc-комментарием (Linux only)
- [ ] добавить `dino` в `ValidateOverlayStyle`, текст ошибки и doc-комментарии (список стилей у поля и у функции)
- [ ] описать `dino` в шаблоне конфига (блок `overlay_style`) с пометкой «Linux only»
- [ ] прогнать `go test -race ./internal/config` — должно пройти

### Task 8: CLI и отказ на macOS

**Files:**
- Modify: `cmd/dndmode/cli.go`
- Modify: `cmd/dndmode/usage_linux.go`
- Modify: `cmd/dndmode/main_linux.go`
- Modify: `cmd/dndmode/main.go`

- [ ] обновить строку `--style` в `cli.go` и `usage_linux.go`, списки стилей в сообщениях `main_linux.go`; `parseStyleFlag` не трогать — `dino` уже проходит как есть
- [ ] в darwin `main.go` после `NormalizeOverlayStyle`: `dino` → сообщение «not available on macOS» с указанием источника (flag/config) по образцу `main_linux.go` и `exitConfigErr`; darwin-строки usage не трогать
- [ ] проверить darwin-правку чтением на типобезопасность (здесь не собирается); тесты `cmd/dndmode` — darwin-only, прогоняются на маке (Post-Completion)
- [ ] прогнать `go build ./... && go vet ./cmd/dndmode` — должно пройти

### Task 9: Проверка критериев приёмки
- [ ] все требования из Overview реализованы
- [ ] граничные случаи: смена размера окна, несколько мониторов, длинный `dt` после засыпания
- [ ] полный прогон: `make test`
- [ ] линтеры: `make lint`
- [ ] сборка и прогон: `make build-linux && ./dndmode --style dino --timer 30s --debug`

### Task 10: [Final] Документация
- [ ] README: таблица флагов (`--style`), копия шаблона конфига (блок `overlay_style`), раздел-описание стиля по образцу bamboo, Linux-раздел и остальные перечисления стилей (`grep -n bamboo README.md`)
- [ ] `docs/index.html` (сайт): перечисление стилей не трогаем — сайт про macOS-релиз
- [ ] локальный CLAUDE.md (в `.gitignore`): `dino` в «Currently ported», что dino, как и bamboo, шагает со своей частотой
- [ ] перенести план в `docs/plans/completed/`

## Post-Completion
*Ручные и внешние шаги, без чекбоксов*

**Ручная проверка:**
- `dndmode --style dino --timer 1m` на 1080p и 4K, несколько мониторов, горячее подключение монитора
- длинный прогон (10+ мин): скорость на потолке, движение ровное, без столкновений; CPU — проверить `top`, ориентир: не выше `gold` (тоже 60 Гц)
- `dndmode --watch --style dino`: щит по Ctrl+Alt+Super+D, разблокировка кодом

**darwin (на маке):**
- `make build && make test` — darwin-тесты `cmd/dndmode` и config
- `./dndmode --style dino` и `overlay_style: dino` → exit 1 с понятным сообщением
