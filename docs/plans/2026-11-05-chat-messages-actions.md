# Реализация работы с сообщениями в Ligament Chat: Ответ, Пересылка, Удаление (у себя / у всех), Редактирование и Разграничение Прав

> **Execution:** Follow this plan task-by-task within the user-authorized scope.

**Goal:** Реализовать полный цикл работы с сообщениями в корпоративном мессенджере Ligament Chat (`ligament-chat`, `2fa` Web и `ligament-apps` Flutter): ответ (reply) с цитированием, пересылка (forward) с сохранением авторства, удаление сообщений (delete for me / delete for everyone) со строгим контролем прав, редактирование (edit), отображение статусов и синхронизация в реальном времени через WebSocket.

**Architecture:** 
1. PostgreSQL в `ligament-chat` хранит сообщения, их правки (`edited_at`), удаление для всех (`deleted_at`) и персональные скрытия в таблице `message_deletions (user_id, message_id, chat_id, deleted_at)`.
2. Запросы `ListMessages` фильтруют удалённые сообщения (`deleted_at IS NULL` и `NOT IN message_deletions`) и обогащают ответы превью цитируемого сообщения (`reply_to`) через `LEFT JOIN`.
3. REST API предоставляет защищённые эндпоинты `DELETE /api/v1/chats/{id}/messages/{msg_id}` (с проверкой прав: автор, админ, модератор для «у всех», любой участник для «у себя»), `PUT /api/v1/chats/{id}/messages/{msg_id}` (редактирование автором) и безопасную пересылку с валидацией источника.
4. WebSocket транслирует события `chat.message_deleted` и `chat.message_updated`.
5. Web-клиент (`chat.js`) и Flutter-приложение (`ligament-apps`) реализуют контекстное меню, плашки ответа/редактирования над полем ввода, модальное окно пересылки и диалог выбора типа удаления с учётом прав.

**Tech Stack:** Go 1.22+ (`pgx/v5`, `net/http`), PostgreSQL 16+, JavaScript ES6+ (Web chat), Flutter / Dart 3 (`provider`, `flutter_webrtc`).

---

### Task 1: Миграция БД и модели данных в `ligament-chat`

**Files:**
- Create: `/Users/aleksey/Documents/ligament-chat/migrations/0005_message_deletions.sql`
- Modify: `/Users/aleksey/Documents/ligament-chat/internal/store/models.go`

**Step 1: Создать SQL-миграцию таблицы `message_deletions`**
Создать таблицу для хранения персональных удалений («удалить у себя»):
```sql
CREATE TABLE IF NOT EXISTS message_deletions (
    user_id VARCHAR(64) NOT NULL,
    message_id BIGINT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    chat_id UUID NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    deleted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_message_deletions_user_chat ON message_deletions(user_id, chat_id);
```

**Step 2: Добавить структуру `ReplyPreview` и поля в `store.Message`**
В `internal/store/models.go` объявить:
```go
type ReplyPreview struct {
    ID         int64  `json:"id"`
    SenderID   string `json:"sender_id"`
    SenderName string `json:"sender_name"`
    Body       string `json:"body"`
}
```
И добавить `ReplyTo *ReplyPreview` в `Message`.

---

### Task 2: Реализация операций в Store (`ligament-chat/internal/store/store.go`)

**Files:**
- Modify: `/Users/aleksey/Documents/ligament-chat/internal/store/store.go`
- Test: `/Users/aleksey/Documents/ligament-chat/internal/store/messages_test.go`

**Step 1: Написать тесты для операций над сообщениями**
Создать тесты:
- Удаление автором для всех -> скрыто для всех.
- Удаление другим пользователем для всех -> `ErrForbidden`.
- Удаление другим пользователем у себя -> скрыто только для него, видно автору.
- Редактирование автором -> `edited_at` обновлён.
- Ответ на сообщение -> `ReplyTo` заполнен.

**Step 2: Реализовать `DeleteMessage` в `Store`**
```go
func (s *Store) DeleteMessage(ctx context.Context, chatID uuid.UUID, messageID int64, userID string, forEveryone bool, isAdminOrMod bool) (*Message, error)
```
- Получает сообщение из БД.
- Если `forEveryone`: проверяет `msg.SenderID == userID || isAdminOrMod`. При нарушении возвращает `ErrForbidden`. Устанавливает `deleted_at = NOW()`.
- Если `!forEveryone`: вставляет запись в `message_deletions`.

**Step 3: Реализовать `EditMessage` в `Store`**
```go
func (s *Store) EditMessage(ctx context.Context, chatID uuid.UUID, messageID int64, userID string, newBody string) (*Message, error)
```
- Проверяет `msg.SenderID == userID` и `deleted_at == nil`.
- Обновляет `body` и `edited_at = NOW()`.

**Step 4: Обновить `ListMessages` и `SendMessage` в `Store`**
- В `ListMessages` принимать `forUserID string`.
- Фильтровать `deleted_at IS NULL` и `NOT EXISTS (SELECT 1 FROM message_deletions ...)`.
- Добавить `LEFT JOIN messages r ON m.reply_to_id = r.id AND r.deleted_at IS NULL` для формирования `ReplyTo`.
- В `SendMessage` при наличии `reply_to_id` подтягивать превью для возвращаемого объекта и WebSocket broadcast.

---

### Task 3: REST API и WebSocket события в `ligament-chat`

**Files:**
- Modify: `/Users/aleksey/Documents/ligament-chat/internal/api/handlers_chat.go`
- Modify: `/Users/aleksey/Documents/ligament-chat/internal/api/server.go`
- Test: `/Users/aleksey/Documents/ligament-chat/internal/api/messages_api_test.go`

**Step 1: Зарегистрировать маршруты в `server.go`**
```go
s.mux.Handle("DELETE /api/v1/chats/{id}/messages/{msg_id}", s.authMiddleware(http.HandlerFunc(s.handleDeleteMessage)))
s.mux.Handle("PUT /api/v1/chats/{id}/messages/{msg_id}", s.authMiddleware(http.HandlerFunc(s.handleEditMessage)))
```

**Step 2: Реализовать `handleDeleteMessage`**
- Валидация прав: `CanAccessChat`.
- `isAdminOrMod = (role == "admin" || role == "moderator" || claims.Role == "admin" || claims.Role == "superadmin")`.
- Вызов `s.st.DeleteMessage`.
- При `forEveryone`: `s.hub.BroadcastToRoom("chat:"+chatID.String(), HubMessage{ Type: "chat.message_deleted", ... })`.
- При `!forEveryone`: `s.hub.SendDirect(userID, HubMessage{ Type: "chat.message_deleted", ... })`.

**Step 3: Реализовать `handleEditMessage`**
- Валидация прав и тела (`{"body": "..."}`).
- Вызов `s.st.EditMessage`.
- `s.hub.BroadcastToRoom("chat:"+chatID.String(), HubMessage{ Type: "chat.message_updated", ... })`.

**Step 4: Поддержка безопасной пересылки в `handleSendMessage`**
- Если передан `forward_message_id`:
  - Сервер загружает оригинальное сообщение.
  - Проверяет доступ пользователя к чату оригинала.
  - Заполняет `Metadata["forward"] = { "sender_id": ..., "sender_name": ..., "message_id": ... }`.
  - Копирует текст/вложения оригинала, если в запросе не передан новый текст.

**Step 5: Запустить тесты и сборку `ligament-chat`**
`go test ./...` и `go vet ./...`.

---

### Task 4: Реализация в Web-интерфейсе (`2fa/internal/web/static/chat.js` и CSS)

**Files:**
- Modify: `/Users/aleksey/Documents/2fa/internal/web/static/chat.js`
- Modify: `/Users/aleksey/Documents/2fa/internal/web/static/chat.css`

**Step 1: Плашка цитирования (Reply Bar) и редактирования (Edit Bar)**
- Разместить над полем ввода контейнер `#chat-reply-preview` с текстом цитируемого сообщения и кнопкой `✕`.
- Поддержать режим редактирования: поле ввода заполняется исходным текстом, плашка сообщает «Редактирование сообщения», отправка вызывает `PUT`.

**Step 2: Контекстное меню действий сообщения**
- По клику на кнопку действий `...` (или по правому клику/долгому тапу) открывать выпадающее меню:
  - ↩️ Ответить
  - ➡️ Переслать
  - ✏️ Редактировать (если автор)
  - 📋 Копировать текст
  - 🗑️ Удалить

**Step 3: Модальное окно подтверждения удаления**
- Если сообщение своё (или пользователь админ/модератор):
  - Показать опции: «Удалить для всех» и «Удалить только у меня».
- Если чужое:
  - Показать опцию: «Удалить только у меня».
- Запрос `DELETE /apps/chat/api/v1/chats/${chatID}/messages/${msgID}?for_everyone=${forEveryone}`.

**Step 4: Модальное окно пересылки (Forward Modal)**
- Список диалогов пользователя с быстрым поиском.
- Отправка в выбранный чат с флагом пересылки.

**Step 5: Отрисовка цитат, пересылок и статуса «(изменено)» в пузырях сообщений**
- Блок цитаты с кликом для плавного скролла к исходному сообщению.
- Заголовок «➡️ Переслано от: [Имя]».
- Метка «(изм.)» рядом со временем отправки.

**Step 6: Обработка WebSocket событий `chat.message_deleted` и `chat.message_updated`**
- Удаление DOM-элемента сообщения или обновление его текста в реальном времени.

---

### Task 5: Реализация во Flutter-клиенте (`ligament-apps`)

**Files:**
- Modify: `/Users/aleksey/Documents/ligament-apps/lib/models/chat_models.dart`
- Modify: `/Users/aleksey/Documents/ligament-apps/lib/services/chat_service.dart`
- Modify: `/Users/aleksey/Documents/ligament-apps/lib/screens/chat_screen.dart`

**Step 1: Обновить модель `ChatMessage`**
- Добавить `ReplyPreview? replyTo`, `DateTime? editedAt`, `Map<String, dynamic>? forwardInfo`.

**Step 2: Методы в `ChatService`**
- `deleteMessage(api, chatId, messageId, {bool forEveryone = false})`.
- `editMessage(api, chatId, messageId, newBody)`.
- `forwardMessage(api, targetChatId, targetTopicId, originalMessage)`.
- В `_handleSocketEvent`:
  - `chat.message_deleted`: удаление из списка `_messages`.
  - `chat.message_updated`: замена объекта в `_messages`.

**Step 3: Контекстное меню и диалоги в `ChatScreen`**
- BottomSheet / PopupMenu при долгом тапе: Ответить, Переслать, Редактировать, Удалить.
- Диалог выбора типа удаления с проверкой роли пользователя.
- Диалог выбора целевого чата для пересылки.
- Плашка ответа/редактирования над строкой ввода.
- Отрисовка превью цитаты, пересылки и «(изменено)» на виджете сообщения.

**Step 4: Проверка Flutter**
- `flutter analyze` (должно быть 0 ошибок).
- `flutter test`.

---

### Task 6: Релизная сборка и выгрузка образов

**Step 1: `ligament-chat` Docker Build & Push**
- Локальная сборка x64:
  `docker buildx build --platform linux/amd64 -t aligorov/ligament_chat:latest ...`
- Push на Docker Hub.

**Step 2: `2fa` Docker Build & Push**
- Локальная сборка x64:
  `docker buildx build --platform linux/amd64 -t aligorov/ligament_2fa:latest ...`
- Push на Docker Hub.

**Step 3: `ligament-apps` Release Tag**
- Повысить версию в `pubspec.yaml`, закоммитить, создать тег `v1.1.234` и отправить на GitHub Actions.
