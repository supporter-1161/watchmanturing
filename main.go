package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const stateFile = "state.json"

// Config — настройки из config.json
type Config struct {
	BotToken          string `json:"bot_token"`            // Токен бота (Telegram или Pusk)
	APIURL            string `json:"api_url"`              // База Pusk, например https://pusk.example.com. Пусто = Telegram
	Channel           string `json:"channel"`              // Имя канала Pusk для уведомлений. Пусто = чат с ботом
	CheckCursorMillis string `json:"check_cursor_setting"` // Интервал проверки курсора (мс)
}

// State — то, что переживает перезапуск
type State struct {
	ChatID int64 `json:"chat_id"`
}

var (
	bot           *tgbotapi.BotAPI
	apiBase       string // база Pusk без /bot... (для нестандартных методов)
	botToken      string
	channelName   string // канал для уведомлений (Pusk-расширение sendChannel)
	chatID        int64
	chatMutex     sync.Mutex
	cursorWatcher *CursorWatcher
	watcherMutex  sync.Mutex // сериализует старт/стоп из апдейтов
)

// loadConfig читает config.json
func loadConfig(filename string) (*Config, error) {
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		return nil, fmt.Errorf("файл конфигурации %s не найден", filename)
	}

	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("ошибка при открытии файла конфигурации: %v", err)
	}
	defer file.Close()

	var config Config
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return nil, fmt.Errorf("ошибка при чтении файла конфигурации: %v", err)
	}

	if config.BotToken == "" {
		return nil, fmt.Errorf("токен бота не задан в конфигурации")
	}

	return &config, nil
}

// apiEndpoint превращает базу Pusk в шаблон, который ждёт go-telegram-bot-api.
// Пустая строка = официальный api.telegram.org.
func apiEndpoint(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return tgbotapi.APIEndpoint
	}
	return base + "/bot%s/%s"
}

// setChatID запоминает чат и сохраняет его на диск
func setChatID(id int64) {
	if id == 0 {
		return
	}
	chatMutex.Lock()
	changed := chatID != id
	chatID = id
	chatMutex.Unlock()

	if changed {
		if err := saveState(State{ChatID: id}); err != nil {
			log.Printf("не удалось сохранить состояние: %v", err)
		}
	}
}

func currentChatID() int64 {
	chatMutex.Lock()
	defer chatMutex.Unlock()
	return chatID
}

func loadState() State {
	var st State
	data, err := os.ReadFile(stateFile)
	if err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func saveState(st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0o600)
}

// sendToChannel отправляет сообщение в канал Pusk (нестандартный метод sendChannel)
func sendToChannel(name, text string) error {
	payload, err := json.Marshal(map[string]string{"channel": name, "text": text})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/bot%s/sendChannel", apiBase, botToken)
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("ответ не разобран (HTTP %d): %w", resp.StatusCode, err)
	}
	if !out.OK {
		return fmt.Errorf("pusk: %s (HTTP %d)", out.Description, resp.StatusCode)
	}
	return nil
}

// notify отправляет уведомление: в канал, если он задан, иначе в чат с ботом
func notify(text string) {
	if bot == nil {
		return
	}

	if channelName != "" && apiBase != "" {
		err := sendToChannel(channelName, text)
		if err == nil {
			return
		}
		log.Printf("канал %q недоступен, шлю в чат: %v", channelName, err)
	}

	id := currentChatID()
	if id == 0 {
		log.Printf("уведомление не отправлено (чат неизвестен): %s", text)
		return
	}
	if _, err := bot.Send(tgbotapi.NewMessage(id, text)); err != nil {
		log.Printf("не удалось отправить уведомление: %v", err)
	}
}

func send(id int64, text string) {
	if _, err := bot.Send(tgbotapi.NewMessage(id, text)); err != nil {
		log.Printf("не удалось отправить сообщение: %v", err)
	}
}

// mainMenu — инлайн-клавиатура (Pusk понимает только inline_keyboard,
// обычную reply-клавиатуру он игнорирует)
func mainMenu() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Проверка", "check"),
			tgbotapi.NewInlineKeyboardButtonData("Блокировка", "lock"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Режим охраны", "guard"),
		),
	)
}

func sendMenu(id int64, text string) {
	msg := tgbotapi.NewMessage(id, text)
	msg.ReplyMarkup = mainMenu()
	if _, err := bot.Send(msg); err != nil {
		log.Printf("не удалось отправить меню: %v", err)
	}
}

func main() {
	config, err := loadConfig("config.json")
	if err != nil {
		log.Fatalf("Ошибка при загрузке конфигурации: %v", err)
	}

	checkMillis, err := strconv.Atoi(config.CheckCursorMillis)
	if err != nil || checkMillis <= 0 {
		log.Fatalf("Неверное значение check_cursor_setting в конфигурации: %v", err)
	}

	// Токен и адрес бота: Pusk или Telegram
	botToken = config.BotToken
	apiBase = strings.TrimRight(strings.TrimSpace(config.APIURL), "/")
	channelName = strings.TrimSpace(config.Channel)

	botInstance, err := tgbotapi.NewBotAPIWithAPIEndpoint(config.BotToken, apiEndpoint(config.APIURL))
	if err != nil {
		log.Fatalf("Ошибка при создании бота: %v", err)
	}
	bot = botInstance

	if apiBase == "" {
		log.Printf("Бэкенд: Telegram (api.telegram.org)")
	} else {
		log.Printf("Бэкенд: Pusk (%s)", apiBase)
	}
	log.Printf("Authorized on account %s", bot.Self.UserName)

	if channelName != "" {
		log.Printf("Уведомления: канал %q", channelName)
	} else {
		log.Printf("Уведомления: чат с ботом")
	}

	// Восстанавливаем чат, чтобы охрана работала сразу после перезапуска
	if st := loadState(); st.ChatID != 0 {
		setChatID(st.ChatID)
		log.Printf("Чат из state.json: %d", st.ChatID)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30

	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		// Нажатия на инлайн-кнопки
		if cb := update.CallbackQuery; cb != nil {
			handleCallback(cb, checkMillis)
			continue
		}

		msg := update.Message
		if msg == nil {
			continue
		}

		who := "unknown"
		if msg.From != nil {
			who = msg.From.UserName
			if who == "" {
				who = msg.From.FirstName
			}
		}
		log.Printf("[%s] %s", who, msg.Text)
		setChatID(msg.Chat.ID)

		// Pusk не всегда присылает entities, поэтому /start распознаём вручную
		if isStartCmd(msg.Text) || (msg.IsCommand() && msg.Command() == "start") {
			sendMenu(msg.Chat.ID, "Добро пожаловать! Выберите действие:")
			continue
		}

		// Текстовые команды оставил для привычки — работают и в Pusk
		switch msg.Text {
		case "Проверка":
			handleCheck(msg.Chat.ID)
		case "Блокировка":
			handleLock(msg.Chat.ID)
		case "Режим охраны":
			handleGuardMode(msg.Chat.ID, checkMillis)
		default:
			sendMenu(msg.Chat.ID, "Неизвестная команда. Выберите действие:")
		}
	}
}

// isStartCmd распознаёт /start (и /start@botname) без опоры на entities
func isStartCmd(text string) bool {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return false
	}
	t = strings.TrimPrefix(t, "/")
	if i := strings.IndexAny(t, " @"); i >= 0 {
		t = t[:i]
	}
	return t == "start"
}

// handleCallback обрабатывает нажатия на инлайн-кнопки
func handleCallback(cb *tgbotapi.CallbackQuery, checkMillis int) {
	id := currentChatID()
	if cb.Message != nil {
		id = cb.Message.Chat.ID
		setChatID(id)
	}
	log.Printf("callback: data=%q chat=%d", cb.Data, id)

	answer := ""
	switch cb.Data {
	case "check":
		handleCheck(id)
	case "lock":
		handleLock(id)
	case "guard":
		handleGuardMode(id, checkMillis)
	default:
		answer = "Неизвестная кнопка"
	}

	if _, err := bot.Request(tgbotapi.NewCallback(cb.ID, answer)); err != nil {
		log.Printf("не удалось ответить на callback: %v", err)
	}
}

// handleCheck — кнопка «Проверка»
func handleCheck(id int64) {
	send(id, "Выполняю проверку состояния экрана...")
	send(id, getScreenStatus())
}

// handleLock — кнопка «Блокировка»
func handleLock(id int64) {
	if isScreenLocked() {
		send(id, "Экран уже заблокирован.")
		return
	}

	send(id, "Выполняю блокировку системы...")

	if err := lockScreen(); err != nil {
		send(id, "Ошибка при блокировке экрана: "+err.Error())
		return
	}
	send(id, "Система заблокирована.")
}

// handleGuardMode — вкл/выкл режим охраны
func handleGuardMode(id int64, checkMillis int) {
	watcherMutex.Lock()
	if cursorWatcher == nil {
		cursorWatcher = NewCursorWatcher(checkMillis, 500, 300)
	}
	if cursorWatcher.IsRunning() {
		cursorWatcher.Stop()
		watcherMutex.Unlock()
		log.Printf("режим охраны выключен")
		send(id, "Режим охраны деактивирован.")
		return
	}
	cursorWatcher.Start()
	watcherMutex.Unlock()

	log.Printf("режим охраны включён")
	send(id, "Режим охраны активирован. Отслеживаю курсор...")
}
