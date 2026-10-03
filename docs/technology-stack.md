# Технологический стек

Сверено с кодом 4 октября 2026 года. Версии закреплены в `go.mod`, `go.sum`, `app/bun.lock`,
`app/src-tauri/Cargo.lock` и `compose.yaml`. Практическая настройка описана в
[руководстве запуска](running.md) и [desktop README](../app/README.md).

| Область                   | Реализация                                           | Назначение                                               |
| ------------------------- | ---------------------------------------------------- | -------------------------------------------------------- |
| Движок, API, CLI и helper | Go; Cobra для CLI                                    | Сетевые операции, управление исполнением, команды        |
| YAML                      | `go.yaml.in/yaml/v3` и собственные строгие проверки  | Ограниченный профиль YAML 1.2 с точной диагностикой      |
| JSON Schema               | `santhosh-tekuri/jsonschema/v6`, Draft 2020-12       | Структура документов, входы и выходы                     |
| Выражения                 | `google/cel-go`                                      | Ограниченные вычисления без внешнего I/O                 |
| Оркестрация               | Temporal Go SDK                                      | Workflow, Activities, история, ожидания и восстановление |
| API                       | HTTP/JSON, OpenAPI и SSE                             | Общая граница CLI, desktop и browser preview             |
| Данные движка             | PostgreSQL + `pgx/v5`                                | Миграции, операции, запросы, проекции и метаданные       |
| Модели                    | Собственный HTTP-адаптер Ollama                      | Проверка capabilities и агентный цикл                    |
| MCP                       | Официальный `modelcontextprotocol/go-sdk`            | Streamable HTTP и stdio в sandbox                        |
| Sandbox                   | Docker Engine API через Unix socket; Linux Go helper | Файлы, процессы, лимиты, сеть и watchdog                 |
| Артефакты и payload       | Локальные файлы + метаданные PostgreSQL              | Байты вне истории Temporal                               |
| Диагностика               | JSON slog + OpenTelemetry OTLP/HTTP                  | Трассы, счётчик HTTP и длительность запросов             |
| Интерфейс                 | React, TypeScript, Vite                              | Редактор, граф, запуски, review и артефакты              |
| Редактор и граф           | CodeMirror, React Flow + Dagre                       | YAML и визуальные связи                                  |
| Native desktop            | Tauri 2, Rust, reqwest, rusqlite                     | HTTP/SSE, SQLite, проверка файлов и системные диалоги    |
| Frontend-инструменты      | Bun, Vitest, Playwright, Prettier                    | Зависимости, проверки и форматирование                   |
| Инфраструктура разработки | Docker Compose                                       | PostgreSQL и Temporal development server                 |

## Границы компонентов

Пользовательский код выполняется в образе sandbox. Python и другие зависимости кубика
устанавливаются в этот образ, а не в процесс Go-движка. Docker Go SDK не используется: клиент Engine
API реализован в `internal/adapters/docker.go`. Docker CLI используется для определения контекста и
подготовки образов.

YAML-библиотека предоставляет дерево с позициями исходника. Knotra дополнительно отклоняет aliases,
anchors, duplicate keys, запрещённые теги и числовые формы. JSON Schema дополняется семантическим
компилятором; один библиотечный parse не является проверкой контракта. Правила заданы в
[нотации](notation/validation.md).

Workflow управляет графом детерминированно. Внешние операции выполняют Activities. Temporal
сохраняет историю, PostgreSQL — принятые команды и проекции, файловый каталог — артефакты и крупные
payload. Для восстановления нужны все три слоя. Снимок workflow не восстанавливает автоматически
файловую систему агента. Подробности — в [архитектуре](architecture.md) и [Temporal](temporal.md).

Первый модельный адаптер — Ollama. OpenAI, Anthropic и S3 остаются направлениями развития.
Конфигурация другого `provider` отклоняется при admission. Транспортная совместимость API сама по
себе не гарантирует одинаковые capabilities моделей.

Desktop сохраняет черновики и клиентские квитанции в SQLite; browser preview — в localStorage.
Авторитетное исполнение всегда находится в Go-движке. Demo исполняет только подготовленные локальные
данные. [API](api/desktop-v1.md) фиксирует одинаковый протокол для клиентов.

## Поставка и проверка

API и worker сейчас работают в одном процессе на одном хосте. Compose запускает инфраструктуру
разработки отдельно; он не является production-рецептом. Удалённый доступ требует HTTPS и bearer
token. Для независимых пользователей нужны отдельные решения доступа, хранения и усиленной изоляции.

Go проверяется `make check`, frontend — Vitest, TypeScript build, Prettier и Playwright, native-код
— `cargo test --locked` и rustfmt. Python-скрипты используют Black; вспомогательная проверка
fixtures имеет собственный requirements-файл. Точные команды и реальные сценарии приведены в
[CONTRIBUTING](../CONTRIBUTING.md) и [отчёте о проверках](verification.md).
