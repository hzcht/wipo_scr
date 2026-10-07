# WIPO Madrid Monitor Collector — Инструкция разработчика

## Архитектура

```
main.go          — точка входа, конфиг, пул прокси, запуск воркеров
worker.go        — жизненный цикл попытки, ретраи, ватчдог, прокси-ротация
navigate.go      — браузерный путь: openHome, submitSearch, waitOutcome, пагинация
internal/replay  — HTTP-движок (POST select.jsp, LZ-string, парсинг JSON)
internal/site    — селекторы, парсеры грида/пейджера, классификатор страниц
internal/browser — запуск Chromium, профили, фингерпринты, stealth
internal/proxy   — пул с EWMA-замером латенси, скейлинг таймаутов
internal/queue   — таски, шардинг страниц, шард-гейт, done/failed хранилища
internal/sink    — запись results.txt, trademarks.tsv, чекпоинты, дедуп
internal/config  — TOML + флаги, валидация, логирование
internal/rules   — файл errors.lst: маркеры "action@::@text" для авто-реакций
```

## Ключевые инварианты

1. **Режим replay — дефолт**. Browser запускается лениво (фоллбэк на `ErrNotJSON` / `ErrAppError`).
2. **Чекпоинт пишется ПОСЛЕ строк** — потеря чекпоинта = ре-волк (дедуп поглощает), никогда не пропуск.
3. **Дедуп — первый вхождение** (`unique` как в JS гриде).
4. **Прокси и браузер поворачиваются вместе** — тёплые куки живут только на своём IP.
5. **Таймауты масштабируются** по EWMA латенси эндпоинта (`proxy.Pool.ScaleTimeout`).
6. **Ватчдог — тишина, а не суммарное время**. Битится на каждом шаге/странице.

## Добавление нового режима ошибки (как `KindAppError`)

1. `internal/site/pagecheck.go`: константа, `String()`, `Faulty()`, маркеры, `Classify` **до** остальных.
2. `internal/site/pagecheck.go`: `IsAppError(text)` + `ErrAppError` sentinel.
3. `internal/replay/client.go` (`post`): проверка `site.IsAppError(raw)` **до** статус-кода/JSON.
4. `internal/replay/parse.go` (`ParsePage`): `site.IsAppError(w.Error)` на JSON-конверте.
5. `navigate.go`: вызов `classifyPage` в новых wait-циклах (`nextPage`, `gotoPage`, `readPager`, `waitGridIdle`).
4. `worker.go` (`handle`): `case site.KindAppError:` + `errors.Is(res.err, site.ErrAppError)` — рекуе с паузой, **без** зарядки прокси/стрика.
5. Тесты: `pagecheck_test.go` (классификация, `IsAppError`), `parse_test.go` (конверты).

## Добавление нового парсера колонки грида

1. `internal/site/parse.go`: `ParseGrid` → `Row` + `Values()` в правильном порядке.
2. `internal/replay/parse.go`: `rowFromDoc` → маппинг Solr-поля → та же трансформация (highlighting-first, stripHtml, cellValue, unique, 64-char cut, +N).
3. Валидация: `WIPOS_REPLAYVALIDATE=1 go test -tags replayprobe ./internal/replay/ -run Validate` — content/order/count = 0/0/0.

## Сборка и проверки

```bash
go build -o wipos.exe .
gofmt -w .
go vet ./...
go test ./... -timeout 180s
# race не работает (нет gcc)
# U+FFFD scan перед коммитом:
#   Get-ChildItem -Recurse -Include *.go,*.md | % { if ([IO.File]::ReadAllText($_,[Text.Encoding]::UTF8).Contains([char]0xFFFD)) { $_.FullName } }
```

**PowerShell-замены в .go файлах запрещены** — ломают UTF-8 (em-dash, ellipsis). Только editor tool + `gofmt -w .`.

## Отладка

- Лог в `logs/run.log` (ротация 32 Мб, 5 файлов) + stdout.
- `WIPOS_TRACE=1` — логирует строки запросов.
- `WIPOS_REPLAYVALIDATE=1` — прогоняет replay против `trademarks.tsv` (27 запросов / 1055 строк).
- Артефакты реплея: `$env:TEMP\wipos_replayprobe\http_*.json` (сырые ответы).
- JS-эталоны: `$env:TEMP\wipos_js\*.js`, `labels.json`.

## Горячие места при рефакторинге

- `worker.go:handle()` — единая точка принятия решений о ретрае/прокси/блоке. Любая новая ошибка должна пройти через `pageErr` или sentinel.
- `navigate.go:waitOutcome` / `nextPage` / `gotoPage` — классификация **каждый опрос**. Не добавляйте ожиданий без неё.
- `internal/replay/parse.go:rowFromDoc` — порядок приоритетов highlighting/doc/def **строго** как в бандле.
- `internal/proxy/pool.go` — `ScaleTimeout`, `NoteSlow`, `SoftFail`/`Fail` неразрывно связаны с ватчдогом и `handle`.

## CI-подобный чек-лист перед пушем

- [ ] `gofmt -w .` / `go vet ./...` / `go test ./...` зелёные
- [ ] `go build -o wipos.exe .` собирается
- [ ] Нет U+FFFD в исходниках
- [ ] `AGENTS.md` обновлён (селекторы, протокол, прокси, режимы ошибок)
- [ ] `README.md` / `DEVELOPER.md` в синхронизации с поведением