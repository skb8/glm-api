# glm-api

Локальный Go-прокси для личного Z.AI Coding Plan. Поддерживает OpenAI Chat Completions и Anthropic Messages, включая потоковые ответы.

## Запуск

Скачайте и распакуйте сборку для своей платформы из [GitHub Actions](https://github.com/skb8/glm-api/actions/workflows/build.yml). На Linux и macOS выполните `chmod +x glm-api` и запустите `./glm-api`. На Windows запустите `./glm-api.exe`.

Для сборки из исходников нужен Go 1.25+.

```sh
go build -o glm-api .
./glm-api
```

Для сборки на Windows используйте `go build -o glm-api.exe .`.

При первом запуске прокси откроет страницу входа Z.AI и продублирует ссылку в терминале. После входа он запишет токены сессии и собственный случайный API-ключ в `.env` в текущей папке. Терминал покажет ключ и список моделей. На Unix у файла будут права `0600`. При следующих запусках прокси использует тот же ключ. Короткоживущий токен проекта обновляется автоматически и хранится в памяти.

По умолчанию сервер слушает `127.0.0.1:8787`. Флаг `-no-browser` оставляет только ссылку для ручного входа, `-login` запускает повторный вход, `-env` задаёт путь к файлу, `-addr` меняет адрес и порт.

## Подключение клиентов

| Клиент | Базовый URL | Аутентификация |
| --- | --- | --- |
| OpenAI | `http://127.0.0.1:8787/v1` | `Authorization: Bearer <GLM_API_KEY>` |
| Anthropic / Claude Code | `http://127.0.0.1:8787/anthropic` | `x-api-key: <GLM_API_KEY>` или Bearer |

Для OpenAI доступны `/v1/models` и `/v1/chat/completions`. Поддерживаются текст, изображения на моделях с поддержкой изображений, вызовы функций и SSE. Для Anthropic доступны `/anthropic/v1/models`, `/anthropic/v1/messages` и `/anthropic/v1/messages/count_tokens`. Маршруты Messages также работают под `/v1`. Подсчёт токенов передаётся в Z.AI и зависит от поддержки этой операции на стороне провайдера. Для Claude Code задайте `ANTHROPIC_BASE_URL` из таблицы, `ANTHROPIC_AUTH_TOKEN` из терминала и выберите модель GLM через `claude --model GLM-5.3`.

```sh
curl http://127.0.0.1:8787/v1/chat/completions \
  -H 'Authorization: Bearer <GLM_API_KEY>' \
  -H 'Content-Type: application/json' \
  -d '{"model":"GLM-5.3","messages":[{"role":"user","content":"Привет!"}]}'
```

Список моделей берётся из встроенного каталога ZCode для личного Coding Plan. Доступ к конкретной модели определяет аккаунт Z.AI. При первом входе прокси использует существующий ключ проекта `zcode-api-key` или создаёт его для Coding Plan.

Реализация опирается на [контракт входа ZCode](https://github.com/zai-org/ZCode/blob/fec975905a2abd4c07e35c40d51fa097a2258ec9/apps/zcode-cli/docs/design/v2/auth-login.md) и [маршрут шлюза ZCode](https://github.com/zai-org/ZCode/blob/fec975905a2abd4c07e35c40d51fa097a2258ec9/apps/zcode-cli/docs/design/v2/model/provider-endpoint-routing.md).
