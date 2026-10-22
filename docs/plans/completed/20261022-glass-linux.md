# Стиль `glass` на Linux

## Overview
- Порт macOS-стиля `glass`: перед показом щита один снимок рабочего стола, размытый, с лёгким затемнением 15%.
- Снимок берётся через `org.freedesktop.portal.Screenshot` (`interactive=false`), работает на Wayland и X11.
- Щит остаётся непрозрачным окном: показывается статичная картинка, а не живой фон. Блокировка ввода как у `black`.

## Context (from discovery)
- macOS: `internal/macos/cocoa/window_darwin.m` (`captureBlurredDesktopImage`, `kGlassTintAlpha = 0.15`, `kGlassBlurRadius = 16`), пересборка щита для glass пропускается (`controller_darwin.go`).
- Linux: `internal/linux/overlay` (`Controller.build`, `newAnimation`, `Styles`), `cmd/dndmode/main_linux.go` (шаг 5b.1 отбрасывает суффикс `glass:N`).
- Проверка портала на GNOME 46: первый вызов показывает диалог разрешения, ответ сохраняется в PermissionStore (`screenshot`/`screenshot`, app_id `""`), дальше ~0,5 с без диалога. Файл снимка кладётся в `~/Pictures` с растущим номером.

## Solution Overview
- C (`overlay_linux.c`): запрос разрешения из PermissionStore, вызов портала с ожиданием `Response` в собственном `GMainContext` (без реентерабельности основного цикла), загрузка PNG и немедленное удаление файла; кроп с масштабом и показ картинки с затемнением.
- Go (`glass.go`, чистые функции с тестами): границы мониторов, перевод монитора в пиксели снимка, рабочий размер с уменьшением для больших радиусов, три прохода box blur (приближение гаусса).
- `Controller` снимает экран один раз на первой сборке, до показа окон и до захвата ввода (диалог можно нажать). При пересборке после смены мониторов режет тот же снимок.
- Таймаут: 5 с, если разрешение уже есть; 30 с, если будет диалог; `no` в хранилище — сразу чёрный щит.
- Любой сбой снимка: чёрный щит, предупреждение в лог. Прозрачным щит не бывает.

## Implementation Steps

### Task 1: Модель glass (Go)
- [x] тесты `glass_test.go`: границы, кроп (один, два монитора, scale 2, монитор вне снимка), рабочий размер, ширины box, blur (однотонная картинка не меняется, импульс расплывается симметрично, радиус 0 — без изменений)
- [x] `glass.go`

### Task 2: C-примитивы и Controller
- [x] `overlay_linux.c/.h`: разрешение, снимок, кроп, данные поверхности, показ, геометрия монитора
- [x] `Controller`: `glassBlur`, снимок на первой сборке, `glassAnim`, освобождение в `Release`; `glass` в `Styles`
- [x] smoke-тест с `glass`

### Task 3: CLI и документация
- [x] `main_linux.go`: радиус как на macOS, баннер `glass_blur`, сообщения со списком стилей
- [x] `usage_linux.go`, README, шаблон конфига, CLAUDE.md

## Post-Completion
- живой прогон `--style glass` и `--style glass:30`, проверка вспышки/звука при снимке.
