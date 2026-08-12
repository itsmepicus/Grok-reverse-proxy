# Grok Reverse Proxy

<p align="center">
  <a href="#english"><strong>English</strong></a>
  ·
  <a href="#русский"><strong>Русский</strong></a>
</p>

<a id="english"></a>

## English

A small, self-contained reverse proxy for a Grok CLI OAuth session. It exposes
OpenAI- and Anthropic-compatible endpoints while using the Grok Responses API
upstream.

The repository contains no account data, tokens, cookies, or request history.
Runtime credentials stay on the machine where the proxy runs.

> This is an unofficial community project. It is not affiliated with xAI.
> Use it only with accounts you control and in accordance with the applicable
> xAI terms and policies. Upstream endpoints and model names can change.

### Features

- `POST /v1/chat/completions` — OpenAI Chat Completions, including SSE streaming.
- `POST /v1/responses` — OpenAI Responses passthrough.
- `POST /v1/messages` — Anthropic Messages compatibility, including streaming.
- `GET /v1/models` and unauthenticated `GET /healthz`.
- Grok model aliases (`grok-build` → `grok-4.6`, pinned `grok-4.5`, Composer aliases).
- Tool calls, reasoning effort normalization, usage conversion, and images.
- Automatic OAuth refresh with atomic owner-only runtime persistence.
- Multiple account files with least-loaded selection, cooldowns, and retries.
- Mandatory API-key protection whenever the service listens beyond loopback.
- Standard-library-only Go binary, Docker image, tests, and GitHub Actions CI.

### Quick start

Requirements: Go 1.23+ and a valid session created by the official `grok` CLI.

```bash
grok login
cp .env.example .env
```

Replace `GROK_PROXY_API_KEY` in `.env` with a random secret:

```bash
openssl rand -hex 32
set -a; source .env; set +a
go run ./cmd/grok-reverse-proxy
```

The default credential path is `~/.grok/auth.json`; the file is read directly
and is never copied into this repository. The proxy writes refreshed tokens to
the ignored `./data/accounts.json` with filesystem mode `0600`.

Test it:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $GROK_PROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.6",
    "messages": [{"role": "user", "content": "Reply with OK"}]
  }'
```

OpenAI SDKs can use `http://127.0.0.1:8080/v1` as their base URL and the proxy
key as `api_key`. Anthropic SDKs use `http://127.0.0.1:8080` as their base URL
and the same value as `x-api-key`.

### Multiple accounts

Pass comma-separated files or glob patterns. Quote globs so the proxy expands
them itself:

```bash
export GROK_AUTH_FILES="$HOME/.grok/auth.json,$HOME/private-grok-accounts/*.json"
```

Each file must contain an access JWT and refresh token in the same JSON object.
Native Grok CLI `key` + `refresh_token`, `access_token` + `refresh_token`, and
camelCase token names are recognized recursively. Duplicate identities merge.

Do not place account exports inside the Git checkout. The `.gitignore` also
blocks common accidental names as a second line of defense.

### Docker Compose

Create a local `.env`, then run:

```bash
docker compose up --build -d
```

The sample compose file mounts `~/.grok/auth.json` read-only, stores rotated
tokens in a named volume, binds only to localhost, runs as a non-root user, and
uses a read-only root filesystem.

### Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `GROK_PROXY_API_KEY` | empty on loopback only | Client authentication secret |
| `GROK_AUTH_FILES` | `~/.grok/auth.json` | Comma-separated credential files/globs |
| `GROK_STATE_FILE` | `./data/accounts.json` | Private refresh-token state |
| `GROK_LISTEN_ADDR` | `127.0.0.1:8080` | HTTP listener |
| `GROK_UPSTREAM_URL` | `https://cli-chat-proxy.grok.com` | Grok Responses upstream |
| `GROK_TOKEN_URL` | `https://auth.x.ai/oauth2/token` | OAuth refresh endpoint |
| `GROK_OAUTH_CLIENT_ID` | public Grok CLI client ID | OAuth client identifier |
| `GROK_CLIENT_VERSION` | `0.2.93` | Upstream compatibility header |
| `GROK_REFRESH_LEAD` | `10m` | Refresh before token expiry |
| `GROK_REQUEST_TIMEOUT` | `120s` | Total upstream timeout |
| `GROK_MAX_BODY_BYTES` | `8388608` | Client request size limit |
| `GROK_MAX_CONCURRENCY_PER_ACCOUNT` | `1` | Per-account parallel requests |
| `GROK_EGRESS_PROXY` | empty | Explicit HTTP(S)/SOCKS5 egress proxy |
| `GROK_ALLOW_CUSTOM_ENDPOINTS` | `false` | Permit non-xAI/Grok OAuth/upstream hosts |

`HTTP_PROXY` and `HTTPS_PROXY` are deliberately ignored so that OAuth tokens
cannot be routed through an ambient proxy by accident. Set `GROK_EGRESS_PROXY`
explicitly if one is required.

OAuth and inference URLs are restricted to xAI/Grok domains by default. The
custom-endpoint override causes bearer and refresh tokens to be sent to the
configured hosts, so enable it only for infrastructure you control.

### Security notes

- Never commit `.env`, `data/`, `.grok/`, auth files, exported accounts, or keys.
- Put TLS and network access control in front of the service before exposing it.
- Use a long random proxy key and rotate it after any suspected disclosure.
- Logs contain request IDs, opaque account IDs, statuses, and safe error text;
  they do not contain prompts, responses, emails, API keys, or OAuth tokens.
- See [SECURITY.md](SECURITY.md) for the reporting and credential policy.

### Development

```bash
make check
make build
```

No live Grok account is needed for the test suite. Tests use synthetic JWTs and
local HTTP servers only.

---

<a id="русский"></a>

## Русский

<p align="center">
  <a href="#english"><strong>English</strong></a>
  ·
  <a href="#русский"><strong>Русский</strong></a>
</p>

Небольшой автономный reverse proxy для OAuth-сессии Grok CLI. Он предоставляет
OpenAI- и Anthropic-совместимые эндпоинты, используя Grok Responses API в
качестве upstream.

В репозитории нет данных аккаунтов, токенов, cookies или истории запросов.
Рабочие credentials остаются только на машине, где запущен proxy.

> Это неофициальный community-проект, не связанный с xAI. Используйте его только
> со своими аккаунтами и в соответствии с действующими правилами xAI. Upstream-
> эндпоинты и названия моделей могут измениться.

### Возможности

- `POST /v1/chat/completions` — OpenAI Chat Completions, включая SSE streaming.
- `POST /v1/responses` — passthrough для OpenAI Responses.
- `POST /v1/messages` — совместимость с Anthropic Messages и streaming.
- `GET /v1/models` и не требующий авторизации `GET /healthz`.
- Алиасы моделей Grok: `grok-build` → `grok-4.6`, закреплённый `grok-4.5` и варианты Composer.
- Tool calls, нормализация reasoning effort, преобразование usage и изображения.
- Автоматическое обновление OAuth с атомарным сохранением приватного state-файла.
- Пул нескольких аккаунтов с балансировкой, cooldown и повторными попытками.
- Обязательная защита API-ключом при прослушивании не только loopback-интерфейса.
- Go-бинарник без внешних зависимостей, Docker, тесты и GitHub Actions CI.

### Быстрый запуск

Требования: Go 1.23+ и действующая сессия, созданная официальным `grok` CLI.

```bash
grok login
cp .env.example .env
```

Замените `GROK_PROXY_API_KEY` в `.env` на случайный секрет:

```bash
openssl rand -hex 32
set -a; source .env; set +a
go run ./cmd/grok-reverse-proxy
```

По умолчанию credentials читаются напрямую из `~/.grok/auth.json`. Этот файл
не копируется в репозиторий. Обновлённые токены proxy сохраняет в исключённый
из Git файл `./data/accounts.json` с правами `0600`.

Проверка:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $GROK_PROXY_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "grok-4.6",
    "messages": [{"role": "user", "content": "Ответь только OK"}]
  }'
```

Для OpenAI SDK укажите `http://127.0.0.1:8080/v1` как base URL, а proxy-ключ —
как `api_key`. Для Anthropic SDK используйте `http://127.0.0.1:8080` как base
URL и тот же ключ как `x-api-key`.

### Несколько аккаунтов

Передайте пути или glob-шаблоны через запятую. Заключите шаблоны в кавычки,
чтобы их раскрывал сам proxy:

```bash
export GROK_AUTH_FILES="$HOME/.grok/auth.json,$HOME/private-grok-accounts/*.json"
```

Каждый файл должен содержать access JWT и refresh token в одном JSON-объекте.
Рекурсивно распознаются нативные поля Grok CLI `key` + `refresh_token`, поля
`access_token` + `refresh_token` и варианты в camelCase. Дубликаты аккаунтов
объединяются.

Не размещайте экспорты аккаунтов внутри Git checkout. `.gitignore` дополнительно
блокирует распространённые имена таких файлов.

### Docker Compose

Создайте локальный `.env`, затем выполните:

```bash
docker compose up --build -d
```

Пример Compose монтирует `~/.grok/auth.json` только для чтения, сохраняет
ротированные токены в named volume, публикует порт только на localhost,
запускается без root и использует read-only root filesystem.

### Конфигурация

| Переменная | Значение по умолчанию | Назначение |
| --- | --- | --- |
| `GROK_PROXY_API_KEY` | пусто только для loopback | Секрет для авторизации клиентов |
| `GROK_AUTH_FILES` | `~/.grok/auth.json` | Credentials-файлы или glob-шаблоны через запятую |
| `GROK_STATE_FILE` | `./data/accounts.json` | Приватное состояние refresh-токенов |
| `GROK_LISTEN_ADDR` | `127.0.0.1:8080` | Адрес HTTP-сервера |
| `GROK_UPSTREAM_URL` | `https://cli-chat-proxy.grok.com` | Grok Responses upstream |
| `GROK_TOKEN_URL` | `https://auth.x.ai/oauth2/token` | OAuth refresh endpoint |
| `GROK_OAUTH_CLIENT_ID` | публичный ID Grok CLI | OAuth client identifier |
| `GROK_CLIENT_VERSION` | `0.2.93` | Версия в compatibility-заголовке |
| `GROK_REFRESH_LEAD` | `10m` | Обновление токена до истечения срока |
| `GROK_REQUEST_TIMEOUT` | `120s` | Общий upstream timeout |
| `GROK_MAX_BODY_BYTES` | `8388608` | Максимальный размер запроса клиента |
| `GROK_MAX_CONCURRENCY_PER_ACCOUNT` | `1` | Параллельные запросы на аккаунт |
| `GROK_EGRESS_PROXY` | пусто | Явно заданный HTTP(S)/SOCKS5 proxy |
| `GROK_ALLOW_CUSTOM_ENDPOINTS` | `false` | Разрешить OAuth/upstream вне доменов xAI/Grok |

`HTTP_PROXY` и `HTTPS_PROXY` намеренно игнорируются, чтобы OAuth-токены случайно
не ушли через системный proxy. При необходимости явно задайте
`GROK_EGRESS_PROXY`.

OAuth- и inference-адреса по умолчанию ограничены доменами xAI/Grok. При
включении custom-endpoint override bearer- и refresh-токены отправляются на
указанные хосты, поэтому используйте его только для доверенной инфраструктуры.

### Безопасность

- Никогда не коммитьте `.env`, `data/`, `.grok/`, auth-файлы, экспорты аккаунтов
  или ключи.
- Перед публикацией сервиса добавьте TLS и сетевые ограничения доступа.
- Используйте длинный случайный proxy-ключ и меняйте его при подозрении на утечку.
- Логи содержат только request ID, непрозрачные account ID, статусы и безопасные
  сообщения об ошибках. Prompts, responses, email, API-ключи и OAuth-токены не
  записываются.
- Политика безопасности описана в [SECURITY.md](SECURITY.md).

### Разработка

```bash
make check
make build
```

Для тестов не требуется реальный Grok-аккаунт: используются синтетические JWT и
локальные HTTP-серверы.
