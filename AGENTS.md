# Правила разработки и сборки проекта Ligament Chat (`ligament-chat`)

## 1. Сборка Docker-образов и сервисов (Строгое правило)
- **Все Docker-образы (сервер Chat, сервисы)** собираются **СТРОГО ЛОКАЛЬНО под X64 (`--platform linux/amd64`) и ВСЕГДА выгружаются на Docker Hub**:
  ```bash
  # Сборка локального x64-образа
  docker buildx build --platform linux/amd64 --load \
    -t ligament-chat:latest \
    -t ligament-chat:<версия> \
    -t aligorov/ligament_chat:latest \
    -t aligorov/ligament_chat:<версия> \
    --build-arg BUILD_DATE=$(date +%F) \
    --build-arg VERSION=<версия> .

  # Выгрузка на Docker Hub
  docker push aligorov/ligament_chat:<версия>
  docker push aligorov/ligament_chat:latest
  ```
- Локальная сборка x64 через `docker buildx` и последующая выгрузка на Docker Hub обязательны при каждом релизе.

## 2. Сборка клиентских приложений (Строгое правило)
- **Клиентские приложения (Windows MSI, macOS DMG/ZIP, Linux deb/tar.gz, Android APK)** собираются **ТОЛЬКО В GITHUB ACTIONS** в репозитории `ligament-apps`.

## 3. Архитектурные ограничения и Zero-ENV
- **Никаких новых `.env` и ENV-переменных** для прикладных настроек, политик, комнат, каналов или секретов. Все прикладные настройки, правила групп и доверенные ключи хранятся в PostgreSQL (`ligament_chat`) и управляются через защищённый API/веб.
- **Единый контур идентификации**: пользователи, группы, исходные права и лицензии остаются в Core (`2fa`). Аутентификация выполняется через Ed25519 SSO-билеты Core (`aud: ligament-chat`).
- **High-Water Mark статус прочтения**: отметка прочтения топиков хранится в виде указателя `last_read_message_id BIGINT` в `topic_participants`, исключая full-table scans и write amplification.
- **P2P WebRTC и сохранение звонков**: обычные звонки — чистый P2P (через `ligament-ice` STUN/TURN). При включении записи стрим кадров/аудио транслируется в Ingest API `ligament-recoder` (`kind: "call"`).

## 4. Git-гигиена: ветки, коммиты и PR
- **Имена веток**: использовать короткие имена максимум из трёх слов через дефис (например, `chat-ws-hub`, `webrtc-signaling`, `topic-unread-idx`).
- **Сообщения коммитов**: Conventional Commits `type(scope): summary` (`feat`, `fix`, `docs`, `chore`, `refactor`, `test`).

## 5. Анти-оверинжиниринг и стиль кода Go (Anti-Bloat & Pragmatism)
- **Локальность и запрет преждевременных абстракций**: инлайнить логику по месту вызова.
- **Валидация на границах (Boundary Validation)**: валидировать HTTP/WebSocket запросы один раз на внешней границе.
- **Линейный поток управления (Guard Clauses & Early Returns)**: ранний возврат при ошибке.
- **Передача `context.Context`**: `ctx context.Context` всегда передается первым аргументом.
- **Отказ от `any` / `interface{}`**: использовать строгие типы.

## 6. Тестирование и проверки (Checks)
- Обязательные проверки перед коммитом:
  ```bash
  go test ./...
  go vet ./...
  ```
