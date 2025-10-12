package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// MonitorConfig содержит конфигурацию мониторинга
type MonitorConfig struct {
	checkInterval       time.Duration
	requestTimeout      time.Duration
	retryDelay          time.Duration
	failureThreshold    int
	notifyBatchWindow   time.Duration
	maxBatchSize        int
	maxConcurrentChecks int
	maxResponseBodySize int64
	dnsTimeout          time.Duration
	tcpTimeout          time.Duration
}

func loadMonitorConfig() MonitorConfig {
	return MonitorConfig{
		checkInterval:       getEnvDuration("CHECK_INTERVAL", 1*time.Minute),
		requestTimeout:      getEnvDuration("REQUEST_TIMEOUT", 45*time.Second),
		retryDelay:          getEnvDuration("RETRY_DELAY", 5*time.Second),
		failureThreshold:    getEnvInt("FAILURE_THRESHOLD", 3),
		notifyBatchWindow:   getEnvDuration("NOTIFY_BATCH_WINDOW", 10*time.Second),
		maxBatchSize:        getEnvInt("MAX_BATCH_SIZE", 50),
		maxConcurrentChecks: getEnvInt("MAX_CONCURRENT_CHECKS", 10),
		maxResponseBodySize: int64(getEnvInt("MAX_RESPONSE_BODY_SIZE", 1048576)), // 1MB
		dnsTimeout:          getEnvDuration("DNS_TIMEOUT", 5*time.Second),
		tcpTimeout:          getEnvDuration("TCP_TIMEOUT", 10*time.Second),
	}
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if val := os.Getenv(key); val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
		slog.Warn("invalid duration, using default",
			"key", key, "default", defaultVal)
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		var i int
		if _, err := fmt.Sscanf(val, "%d", &i); err == nil && i > 0 {
			return i
		}
		slog.Warn("invalid integer, using default",
			"key", key, "default", defaultVal)
	}
	return defaultVal
}

type Config struct {
	Endpoints []Endpoint `yaml:"endpoints"`
}

type Endpoint struct {
	URL            string            `yaml:"url,omitempty"`      // для HTTP проверок
	Type           string            `yaml:"type,omitempty"`      // http, dns, tcp
	Method         string            `yaml:"method,omitempty"`    // для HTTP
	ExpectedStatus int               `yaml:"expected_status,omitempty"` // для HTTP
	Headers        map[string]string `yaml:"headers,omitempty"`   // для HTTP
	
	// Для DNS проверок
	Host       string `yaml:"host,omitempty"`        // хост для DNS или TCP
	RecordType string `yaml:"record_type,omitempty"` // A, AAAA, CNAME
	Expected   string `yaml:"expected,omitempty"`    // ожидаемое значение (опционально)
	
	// Для TCP проверок
	Port    int    `yaml:"port,omitempty"`    // порт для TCP
	Address string `yaml:"address,omitempty"` // полный адрес для TCP (альтернатива host:port)
}

// GetIdentifier возвращает уникальный идентификатор для endpoint
func (e Endpoint) GetIdentifier() string {
	switch strings.ToLower(e.Type) {
	case "dns":
		return fmt.Sprintf("dns://%s/%s", e.Host, e.RecordType)
	case "tcp":
		if e.Address != "" {
			return fmt.Sprintf("tcp://%s", e.Address)
		}
		return fmt.Sprintf("tcp://%s:%d", e.Host, e.Port)
	default: // http
		return e.URL
	}
}

// ServiceState использует RWMutex для оптимизации чтения
type ServiceState struct {
	mu               sync.RWMutex
	consecutiveFails int
	isDown           bool
	firstFailTime    time.Time
	lastCheckTime    time.Time
}

type Monitor struct {
	config         Config
	monitorConfig  MonitorConfig
	states         sync.Map
	telegramToken  string
	telegramChatID string
	mattermostURL  string
	httpClient     *http.Client
	resolver       *net.Resolver // для DNS проверок
	notifyQueue    chan *NotifyEvent
	semaphore      chan struct{}
	wg             sync.WaitGroup
	eventPool      sync.Pool
}

type NotifyEvent struct {
	endpoint  string
	isDown    bool
	timestamp time.Time
	failTime  time.Time
}

// Notifier интерфейс для унификации отправки уведомлений
type Notifier interface {
	Send(ctx context.Context, message string) error
	Name() string
}

// TelegramNotifier реализация для Telegram
type TelegramNotifier struct {
	token      string
	chatID     string
	httpClient *http.Client
}

func (t *TelegramNotifier) Send(ctx context.Context, message string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token)
	payload := map[string]interface{}{
		"chat_id":    t.chatID,
		"text":       message,
		"parse_mode": "Markdown",
	}
	return sendJSONRequest(ctx, t.httpClient, url, payload)
}

func (t *TelegramNotifier) Name() string {
	return "Telegram"
}

// MattermostNotifier реализация для Mattermost
type MattermostNotifier struct {
	webhookURL string
	httpClient *http.Client
}

func (m *MattermostNotifier) Send(ctx context.Context, message string) error {
	payload := map[string]string{"text": message}
	return sendJSONRequest(ctx, m.httpClient, m.webhookURL, payload)
}

func (m *MattermostNotifier) Name() string {
	return "Mattermost"
}

// sendJSONRequest унифицированная функция с ограничением размера ответа
func sendJSONRequest(ctx context.Context, client *http.Client, targetURL string, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			slog.Warn("failed to close response body", "error", cerr)
		}
	}()

	// Ограничиваем чтение ответа до 1MB для защиты от OOM
	limited := io.LimitReader(resp.Body, 1<<20)
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return fmt.Errorf("drain body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

// maskSensitiveString маскирует чувствительные данные для логов
func maskSensitiveString(s string, showLast int) string {
	if len(s) <= showLast {
		return "***"
	}
	return strings.Repeat("*", len(s)-showLast) + s[len(s)-showLast:]
}

// sanitizeURL удаляет query parameters и credentials из URL для безопасного логирования
func sanitizeURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "[invalid-url]"
	}
	parsed.RawQuery = ""
	parsed.User = nil
	return parsed.String()
}

// validateEndpoint валидирует endpoint перед использованием
func validateEndpoint(ep Endpoint) error {
	endpointType := strings.ToLower(ep.Type)
	if endpointType == "" {
		endpointType = "http"
	}

	switch endpointType {
	case "http":
		if ep.URL == "" {
			return errors.New("URL is required for HTTP endpoint")
		}

		parsed, err := url.Parse(ep.URL)
		if err != nil {
			return fmt.Errorf("invalid URL: %w", err)
		}

		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("URL scheme must be http or https, got: %s", parsed.Scheme)
		}

		if parsed.Host == "" {
			return errors.New("URL must have a host")
		}

		// Проверяем метод
		if ep.Method != "" {
			method := strings.ToUpper(ep.Method)
			validMethods := map[string]bool{
				"GET": true, "POST": true, "PUT": true, "PATCH": true,
				"DELETE": true, "HEAD": true, "OPTIONS": true,
			}
			if !validMethods[method] {
				return fmt.Errorf("invalid HTTP method: %s", ep.Method)
			}
		}

		// Проверяем expected status
		if ep.ExpectedStatus != 0 && (ep.ExpectedStatus < 100 || ep.ExpectedStatus > 599) {
			return fmt.Errorf("invalid expected status code: %d", ep.ExpectedStatus)
		}

	case "dns":
		if ep.Host == "" {
			return errors.New("host is required for DNS endpoint")
		}

		recordType := strings.ToUpper(ep.RecordType)
		if recordType == "" {
			return errors.New("record_type is required for DNS endpoint")
		}

		validTypes := map[string]bool{"A": true, "AAAA": true, "CNAME": true}
		if !validTypes[recordType] {
			return fmt.Errorf("unsupported DNS record type: %s (supported: A, AAAA, CNAME)", recordType)
		}

		// Валидация expected если задано
		if ep.Expected != "" {
			switch recordType {
			case "A":
				if net.ParseIP(ep.Expected) == nil || !strings.Contains(ep.Expected, ".") {
					return fmt.Errorf("invalid IPv4 address in expected: %s", ep.Expected)
				}
			case "AAAA":
				if net.ParseIP(ep.Expected) == nil || !strings.Contains(ep.Expected, ":") {
					return fmt.Errorf("invalid IPv6 address in expected: %s", ep.Expected)
				}
			case "CNAME":
				// CNAME должен быть валидным доменом
				if strings.HasSuffix(ep.Expected, ".") {
					ep.Expected = ep.Expected[:len(ep.Expected)-1]
				}
			}
		}

	case "tcp":
		if ep.Address == "" && ep.Host == "" {
			return errors.New("address or host is required for TCP endpoint")
		}

		if ep.Address == "" {
			if ep.Port <= 0 || ep.Port > 65535 {
				return fmt.Errorf("invalid port: %d", ep.Port)
			}
			ep.Address = fmt.Sprintf("%s:%d", ep.Host, ep.Port)
		}

		// Проверяем что адрес парсится
		if _, _, err := net.SplitHostPort(ep.Address); err != nil {
			return fmt.Errorf("invalid TCP address: %w", err)
		}

	default:
		return fmt.Errorf("unsupported endpoint type: %s", ep.Type)
	}

	return nil
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(); err != nil {
		slog.Error("fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	telegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	telegramChatID := os.Getenv("TELEGRAM_CHAT_ID")
	mattermostURL := os.Getenv("MATTERMOST_WEBHOOK_URL")

	if telegramToken == "" || telegramChatID == "" {
		return errors.New("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID must be set")
	}

	// Валидация Telegram token (базовая проверка формата)
	if !strings.Contains(telegramToken, ":") || len(telegramToken) < 20 {
		return errors.New("TELEGRAM_BOT_TOKEN appears to be invalid")
	}

	// Валидация Mattermost webhook URL если задан
	if mattermostURL != "" {
		parsed, err := url.Parse(mattermostURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("MATTERMOST_WEBHOOK_URL is invalid: %w", err)
		}
	}

	configPath := "config.yaml"
	if cp := os.Getenv("CONFIG_PATH"); cp != "" {
		configPath = cp
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}

	if len(config.Endpoints) == 0 {
		return errors.New("no endpoints configured")
	}

	// Нормализация типов и валидация всех эндпоинтов
	for i, ep := range config.Endpoints {
		if ep.Type == "" {
			config.Endpoints[i].Type = "http"
		}
		config.Endpoints[i].Type = strings.ToLower(config.Endpoints[i].Type)
		
		if err := validateEndpoint(config.Endpoints[i]); err != nil {
			return fmt.Errorf("endpoint %d validation failed: %w", i, err)
		}
	}

	slog.Info("loaded configuration", "endpoints", len(config.Endpoints))
	for _, ep := range config.Endpoints {
		logEndpoint(ep)
	}

	monitorConfig := loadMonitorConfig()
	// Безопасное логирование - маскируем chat_id
	slog.Info("monitor configuration",
		"check_interval", monitorConfig.checkInterval,
		"request_timeout", monitorConfig.requestTimeout,
		"retry_delay", monitorConfig.retryDelay,
		"failure_threshold", monitorConfig.failureThreshold,
		"notify_batch_window", monitorConfig.notifyBatchWindow,
		"max_batch_size", monitorConfig.maxBatchSize,
		"max_concurrent_checks", monitorConfig.maxConcurrentChecks,
		"max_response_body_size", monitorConfig.maxResponseBodySize,
		"dns_timeout", monitorConfig.dnsTimeout,
		"tcp_timeout", monitorConfig.tcpTimeout,
		"telegram_chat_id", maskSensitiveString(telegramChatID, 4),
		"mattermost_enabled", mattermostURL != "")

	// HTTP клиент с оптимизированными параметрами
	httpClient := &http.Client{
		Timeout: monitorConfig.requestTimeout,
		Transport: &http.Transport{
			MaxIdleConns:          20,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       30 * time.Second,
			MaxConnsPerHost:       5,
			DisableKeepAlives:     false,
			DisableCompression:    false,
			ResponseHeaderTimeout: 10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}

	// Создаем резолвер для DNS с оптимальными настройками
	resolver := &net.Resolver{
		PreferGo: true, // Используем Go реализацию для лучшего контроля
		StrictErrors: true,
	}

	// Создаем нотификаторы
	notifiers := []Notifier{
		&TelegramNotifier{
			token:      telegramToken,
			chatID:     telegramChatID,
			httpClient: httpClient,
		},
	}

	if mattermostURL != "" {
		notifiers = append(notifiers, &MattermostNotifier{
			webhookURL: mattermostURL,
			httpClient: httpClient,
		})
		slog.Info("mattermost fallback enabled")
	}

	monitor := &Monitor{
		config:         config,
		monitorConfig:  monitorConfig,
		telegramToken:  telegramToken,
		telegramChatID: telegramChatID,
		mattermostURL:  mattermostURL,
		httpClient:     httpClient,
		resolver:       resolver,
		notifyQueue:    make(chan *NotifyEvent, 100),
		semaphore:      make(chan struct{}, monitorConfig.maxConcurrentChecks),
		eventPool: sync.Pool{
			New: func() interface{} {
				return &NotifyEvent{}
			},
		},
	}

	// Инициализируем состояния в sync.Map
	for _, ep := range config.Endpoints {
		monitor.states.Store(ep.GetIdentifier(), &ServiceState{})
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	monitor.wg.Add(1)
	go monitor.notificationWorker(ctx, notifiers)

	// Горутина для периодического логирования метрик памяти
	monitor.wg.Add(1)
	go monitor.memoryMonitor(ctx)

	slog.Info("starting service monitor",
		"interval", monitorConfig.checkInterval,
		"gomaxprocs", runtime.GOMAXPROCS(0))

	ticker := time.NewTicker(monitorConfig.checkInterval)
	defer ticker.Stop()

	slog.Info("running initial health check")
	monitor.checkAllServices(ctx)
	slog.Info("initial check completed")

	for {
		select {
		case <-ticker.C:
			monitor.checkAllServices(ctx)
		case <-ctx.Done():
			slog.Info("received shutdown signal, stopping gracefully")
			close(monitor.notifyQueue)
			monitor.wg.Wait()
			slog.Info("shutdown complete")
			return nil
		}
	}
}

func logEndpoint(ep Endpoint) {
	switch ep.Type {
	case "dns":
		slog.Info("monitoring endpoint",
			"type", "DNS",
			"host", ep.Host,
			"record_type", strings.ToUpper(ep.RecordType),
			"has_expected", ep.Expected != "")
	case "tcp":
		addr := ep.Address
		if addr == "" {
			addr = fmt.Sprintf("%s:%d", ep.Host, ep.Port)
		}
		slog.Info("monitoring endpoint",
			"type", "TCP",
			"address", addr)
	default: // http
		method := ep.Method
		if method == "" {
			method = http.MethodGet
		}
		expectedStatus := ep.ExpectedStatus
		if expectedStatus == 0 {
			expectedStatus = http.StatusOK
		}
		slog.Info("monitoring endpoint",
			"type", "HTTP",
			"url", sanitizeURL(ep.URL),
			"method", method,
			"expected_status", expectedStatus,
			"has_custom_headers", len(ep.Headers) > 0)
	}
}

// memoryMonitor периодически логирует использование памяти
func (m *Monitor) memoryMonitor(ctx context.Context) {
	defer m.wg.Done()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	var memStats runtime.MemStats

	for {
		select {
		case <-ticker.C:
			runtime.ReadMemStats(&memStats)
			slog.Info("memory stats",
				"alloc_mb", memStats.Alloc/1024/1024,
				"sys_mb", memStats.Sys/1024/1024,
				"num_gc", memStats.NumGC,
				"goroutines", runtime.NumGoroutine())

			// Только предупреждаем при очень высоком использовании (500MB+)
			if memStats.Alloc > 500*1024*1024 {
				slog.Warn("high memory usage detected", "alloc_mb", memStats.Alloc/1024/1024)
			}
		case <-ctx.Done():
			return
		}
	}
}

func (m *Monitor) checkAllServices(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	var wg sync.WaitGroup
	startTime := time.Now()

	for _, ep := range m.config.Endpoints {
		wg.Add(1)
		go func(endpoint Endpoint) {
			defer wg.Done()

			// Используем семафор для строгого контроля конкурентности
			select {
			case m.semaphore <- struct{}{}:
				defer func() { <-m.semaphore }()
				m.checkService(ctx, endpoint)
			case <-ctx.Done():
				return
			}
		}(ep)
	}
	wg.Wait()

	duration := time.Since(startTime)
	slog.Debug("health check cycle completed", "duration", duration.Round(time.Millisecond))
}

func (m *Monitor) checkService(ctx context.Context, ep Endpoint) {
	if ctx.Err() != nil {
		return
	}

	identifier := ep.GetIdentifier()
	success := m.performCheck(ctx, ep)

	if !success && ctx.Err() == nil {
		// Используем контекст с таймаутом для retry
		retryCtx, cancel := context.WithTimeout(ctx, m.monitorConfig.retryDelay+m.monitorConfig.requestTimeout)
		defer cancel()

		time.Sleep(m.monitorConfig.retryDelay)
		success = m.performCheck(retryCtx, ep)
	}

	m.updateState(identifier, success)
}

func (m *Monitor) performCheck(ctx context.Context, ep Endpoint) bool {
	switch ep.Type {
	case "dns":
		return m.performDNSCheck(ctx, ep)
	case "tcp":
		return m.performTCPCheck(ctx, ep)
	default: // http
		return m.performHTTPCheck(ctx, ep)
	}
}

// performDNSCheck выполняет DNS проверку
func (m *Monitor) performDNSCheck(ctx context.Context, ep Endpoint) bool {
	recordType := strings.ToUpper(ep.RecordType)
	startTime := time.Now()

	// Создаем контекст с таймаутом для DNS запроса
	dnsCtx, cancel := context.WithTimeout(ctx, m.monitorConfig.dnsTimeout)
	defer cancel()

	var success bool
	var resultStr string

	switch recordType {
	case "A":
		ips, err := m.resolver.LookupIPAddr(dnsCtx, ep.Host)
		if err != nil {
			slog.Warn("DNS A lookup failed",
				"host", ep.Host,
				"error", err)
			return false
		}

		// Фильтруем только IPv4
		var ipv4s []string
		for _, ip := range ips {
			if ip.IP.To4() != nil {
				ipv4s = append(ipv4s, ip.IP.String())
			}
		}

		if len(ipv4s) == 0 {
			slog.Warn("DNS A lookup returned no IPv4 addresses", "host", ep.Host)
			return false
		}

		resultStr = strings.Join(ipv4s, ", ")
		
		if ep.Expected != "" {
			success = false
			for _, ip := range ipv4s {
				if ip == ep.Expected {
					success = true
					break
				}
			}
			if !success {
				slog.Warn("DNS A record mismatch",
					"host", ep.Host,
					"expected", ep.Expected,
					"got", resultStr)
				return false
			}
		} else {
			success = true
		}

	case "AAAA":
		ips, err := m.resolver.LookupIPAddr(dnsCtx, ep.Host)
		if err != nil {
			slog.Warn("DNS AAAA lookup failed",
				"host", ep.Host,
				"error", err)
			return false
		}

		// Фильтруем только IPv6
		var ipv6s []string
		for _, ip := range ips {
			if ip.IP.To4() == nil && ip.IP.To16() != nil {
				ipv6s = append(ipv6s, ip.IP.String())
			}
		}

		if len(ipv6s) == 0 {
			slog.Warn("DNS AAAA lookup returned no IPv6 addresses", "host", ep.Host)
			return false
		}

		resultStr = strings.Join(ipv6s, ", ")

		if ep.Expected != "" {
			success = false
			for _, ip := range ipv6s {
				if ip == ep.Expected {
					success = true
					break
				}
			}
			if !success {
				slog.Warn("DNS AAAA record mismatch",
					"host", ep.Host,
					"expected", ep.Expected,
					"got", resultStr)
				return false
			}
		} else {
			success = true
		}

	case "CNAME":
		cname, err := m.resolver.LookupCNAME(dnsCtx, ep.Host)
		if err != nil {
			slog.Warn("DNS CNAME lookup failed",
				"host", ep.Host,
				"error", err)
			return false
		}

		// Удаляем завершающую точку для сравнения
		cname = strings.TrimSuffix(cname, ".")
		resultStr = cname

		if ep.Expected != "" {
			expected := strings.TrimSuffix(ep.Expected, ".")
			if cname != expected {
				slog.Warn("DNS CNAME record mismatch",
					"host", ep.Host,
					"expected", expected,
					"got", cname)
				return false
			}
		}
		success = true
	}

	if success {
		duration := time.Since(startTime)
		slog.Debug("DNS check successful",
			"host", ep.Host,
			"type", recordType,
			"result", resultStr,
			"duration", duration.Round(time.Millisecond))
	}

	return success
}

// performTCPCheck выполняет TCP проверку
func (m *Monitor) performTCPCheck(ctx context.Context, ep Endpoint) bool {
	address := ep.Address
	if address == "" {
		address = fmt.Sprintf("%s:%d", ep.Host, ep.Port)
	}

	startTime := time.Now()

	// Создаем dialer с таймаутом
	d := net.Dialer{
		Timeout: m.monitorConfig.tcpTimeout,
	}

	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		slog.Warn("TCP connection failed",
			"address", address,
			"error", err)
		return false
	}
	
	// Закрываем соединение сразу после успешного подключения
	if err := conn.Close(); err != nil {
		slog.Warn("failed to close TCP connection",
			"address", address,
			"error", err)
	}

	duration := time.Since(startTime)
	slog.Debug("TCP check successful",
		"address", address,
		"duration", duration.Round(time.Millisecond))

	return true
}

// performHTTPCheck выполняет HTTP проверку (оригинальный performCheck)
func (m *Monitor) performHTTPCheck(ctx context.Context, ep Endpoint) bool {
	method := ep.Method
	if method == "" {
		method = http.MethodGet
	}

	expectedStatus := ep.ExpectedStatus
	if expectedStatus == 0 {
		expectedStatus = http.StatusOK
	}

	startTime := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, ep.URL, nil)
	if err != nil {
		slog.Error("failed to create request",
			"url", sanitizeURL(ep.URL),
			"error", err)
		return false
	}

	// Пользовательские заголовки имеют приоритет
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		slog.Warn("request failed",
			"url", sanitizeURL(ep.URL),
			"error", err)
		return false
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			slog.Warn("failed to close response body",
				"url", sanitizeURL(ep.URL),
				"error", cerr)
		}
	}()

	// КРИТИЧНО: Ограничиваем чтение тела ответа для защиты от OOM
	limited := io.LimitReader(resp.Body, m.monitorConfig.maxResponseBodySize)
	written, err := io.Copy(io.Discard, limited)
	if err != nil {
		slog.Warn("failed to drain response body",
			"url", sanitizeURL(ep.URL),
			"error", err)
	}

	// Предупреждаем, если тело ответа слишком большое
	if written >= m.monitorConfig.maxResponseBodySize {
		slog.Warn("response body truncated",
			"url", sanitizeURL(ep.URL),
			"size", written,
			"limit", m.monitorConfig.maxResponseBodySize)
	}

	if resp.StatusCode != expectedStatus {
		slog.Warn("unexpected status",
			"url", sanitizeURL(ep.URL),
			"status", resp.StatusCode,
			"expected", expectedStatus)
		return false
	}

	duration := time.Since(startTime)
	// Используем Debug вместо Info для успешных проверок - меньше шума в логах
	slog.Debug("HTTP check successful",
		"url", sanitizeURL(ep.URL),
		"status", resp.StatusCode,
		"response_size", written,
		"duration", duration.Round(time.Millisecond))
	return true
}

func (m *Monitor) updateState(identifier string, success bool) {
	val, ok := m.states.Load(identifier)
	if !ok {
		// Это не должно происходить, но добавляем защиту
		slog.Error("state not found for identifier", "id", identifier)
		return
	}
	state := val.(*ServiceState)

	state.mu.Lock()
	defer state.mu.Unlock()

	now := time.Now()
	state.lastCheckTime = now

	if success {
		if state.isDown {
			// Получаем событие из пула
			event := m.eventPool.Get().(*NotifyEvent)
			event.endpoint = identifier
			event.isDown = false
			event.timestamp = now
			event.failTime = state.firstFailTime

			select {
			case m.notifyQueue <- event:
			default:
				slog.Warn("notification queue full, dropping recovery event",
					"id", identifier)
				m.eventPool.Put(event)
			}

			state.isDown = false
			state.consecutiveFails = 0
			state.firstFailTime = time.Time{}
		} else {
			state.consecutiveFails = 0
		}
	} else {
		state.consecutiveFails++
		if state.consecutiveFails == 1 {
			state.firstFailTime = now
		}

		if state.consecutiveFails >= m.monitorConfig.failureThreshold && !state.isDown {
			state.isDown = true

			// Получаем событие из пула
			event := m.eventPool.Get().(*NotifyEvent)
			event.endpoint = identifier
			event.isDown = true
			event.timestamp = now
			event.failTime = state.firstFailTime

			select {
			case m.notifyQueue <- event:
			default:
				slog.Warn("notification queue full, dropping failure event",
					"id", identifier)
				m.eventPool.Put(event)
			}
		}
	}
}

// notificationWorker оптимизированная версия с упрощенным управлением таймером
func (m *Monitor) notificationWorker(ctx context.Context, notifiers []Notifier) {
	defer m.wg.Done()

	batch := make([]*NotifyEvent, 0, m.monitorConfig.maxBatchSize)
	timer := time.NewTimer(m.monitorConfig.notifyBatchWindow)
	timer.Stop()

	flush := func() {
		if len(batch) > 0 {
			m.sendBatchNotification(ctx, batch, notifiers)
			// Возвращаем события в пул
			for _, event := range batch {
				m.eventPool.Put(event)
			}
			batch = batch[:0]
		}
	}

	for {
		select {
		case event, ok := <-m.notifyQueue:
			if !ok {
				timer.Stop()
				flush()
				return
			}

			batch = append(batch, event)

			// Если достигли максимального размера батча, отправляем сразу
			if len(batch) >= m.monitorConfig.maxBatchSize {
				timer.Stop()
				flush()
			} else if len(batch) == 1 {
				// Запускаем таймер только для первого события
				timer.Reset(m.monitorConfig.notifyBatchWindow)
			}

		case <-timer.C:
			flush()

		case <-ctx.Done():
			timer.Stop()
			flush()
			return
		}
	}
}

func (m *Monitor) sendBatchNotification(ctx context.Context, events []*NotifyEvent, notifiers []Notifier) {
	if len(events) == 0 {
		return
	}

	downServices := make([]string, 0, len(events))
	upServices := make([]string, 0, len(events))
	downDetails := make(map[string]time.Time, len(events))
	upDetails := make(map[string]time.Time, len(events))

	for _, e := range events {
		if e.isDown {
			downServices = append(downServices, e.endpoint)
			downDetails[e.endpoint] = e.failTime
		} else {
			upServices = append(upServices, e.endpoint)
			upDetails[e.endpoint] = e.failTime
		}
	}

	// Точный расчет размера буфера
	estimatedSize := 0
	if len(downServices) > 0 {
		estimatedSize += 25
		for _, svc := range downServices {
			estimatedSize += len(svc) + 40
		}
	}
	if len(upServices) > 0 {
		estimatedSize += 30
		for _, svc := range upServices {
			estimatedSize += len(svc) + 50
		}
	}

	var sb strings.Builder
	sb.Grow(estimatedSize)

	if len(downServices) > 0 {
		sb.WriteString("🔴 *Services DOWN:*\n")
		for _, svc := range downServices {
			sb.WriteString("• ")
			sb.WriteString(svc)
			sb.WriteString("\n  Failed at: ")
			sb.WriteString(downDetails[svc].Format("2006-01-02 15:04:05"))
			sb.WriteString("\n")
		}
	}

	if len(upServices) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("✅ *Services RECOVERED:*\n")
		for _, svc := range upServices {
			failTime := upDetails[svc]
			duration := time.Since(failTime).Round(time.Second)
			sb.WriteString("• ")
			sb.WriteString(svc)
			sb.WriteString("\n  Downtime: ")
			sb.WriteString(duration.String())
			sb.WriteString("\n")
		}
	}

	msg := sb.String()

	// Контекст с таймаутом для уведомлений
	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Пробуем отправить через все доступные нотификаторы
	for _, notifier := range notifiers {
		if err := notifier.Send(notifyCtx, msg); err != nil {
			slog.Warn("notification failed",
				"notifier", notifier.Name(),
				"error", err)
			continue
		}
		slog.Info("notification sent", "notifier", notifier.Name())
		return
	}

	slog.Error("failed to send notification through all channels")
}
