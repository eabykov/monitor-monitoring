package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type MonitorConfig struct {
	checkInterval     time.Duration
	requestTimeout    time.Duration
	retryDelay        time.Duration
	failureThreshold  int
	notifyBatchWindow time.Duration
}

func loadMonitorConfig() MonitorConfig {
	return MonitorConfig{
		checkInterval:     getEnvDuration("CHECK_INTERVAL", 1*time.Minute),
		requestTimeout:    getEnvDuration("REQUEST_TIMEOUT", 45*time.Second),
		retryDelay:        getEnvDuration("RETRY_DELAY", 5*time.Second),
		failureThreshold:  getEnvInt("FAILURE_THRESHOLD", 3),
		notifyBatchWindow: getEnvDuration("NOTIFY_BATCH_WINDOW", 10*time.Second),
	}
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if val := os.Getenv(key); val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
		slog.Warn("invalid duration, using default",
			"key", key, "value", val, "default", defaultVal)
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
			"key", key, "value", val, "default", defaultVal)
	}
	return defaultVal
}

type Config struct {
	Endpoints []Endpoint `yaml:"endpoints"`
}

type Endpoint struct {
	URL            string            `yaml:"url"`
	Method         string            `yaml:"method,omitempty"`
	ExpectedStatus int               `yaml:"expected_status,omitempty"`
	Headers        map[string]string `yaml:"headers,omitempty"`
}

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
	states         map[string]*ServiceState
	statesMu       sync.RWMutex
	telegramToken  string
	telegramChatID string
	mattermostURL  string
	httpClient     *http.Client
	notifyQueue    chan NotifyEvent
	wg             sync.WaitGroup
}

type NotifyEvent struct {
	endpoint  string
	isDown    bool
	timestamp time.Time
	failTime  time.Time
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
		return fmt.Errorf("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID must be set")
	}

	configPath := "config.yaml"
	if cp := os.Getenv("CONFIG_PATH"); cp != "" {
		configPath = cp
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	if len(config.Endpoints) == 0 {
		return fmt.Errorf("no endpoints configured")
	}

	slog.Info("loaded configuration", "endpoints", len(config.Endpoints))
	for _, ep := range config.Endpoints {
		method := ep.Method
		if method == "" {
			method = "GET"
		}
		expectedStatus := ep.ExpectedStatus
		if expectedStatus == 0 {
			expectedStatus = 200
		}
		slog.Info("monitoring endpoint",
			"url", ep.URL,
			"method", method,
			"expected_status", expectedStatus)
	}

	monitorConfig := loadMonitorConfig()
	slog.Info("monitor configuration",
		"check_interval", monitorConfig.checkInterval,
		"request_timeout", monitorConfig.requestTimeout,
		"retry_delay", monitorConfig.retryDelay,
		"failure_threshold", monitorConfig.failureThreshold,
		"notify_batch_window", monitorConfig.notifyBatchWindow)

	monitor := &Monitor{
		config:         config,
		monitorConfig:  monitorConfig,
		states:         make(map[string]*ServiceState),
		telegramToken:  telegramToken,
		telegramChatID: telegramChatID,
		mattermostURL:  mattermostURL,
		httpClient: &http.Client{
			Timeout: monitorConfig.requestTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		notifyQueue: make(chan NotifyEvent, 100),
	}

	for _, ep := range config.Endpoints {
		monitor.states[ep.URL] = &ServiceState{}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	monitor.wg.Add(1)
	go monitor.notificationWorker(ctx)

	slog.Info("starting service monitor",
		"interval", monitorConfig.checkInterval,
		"chat_id", telegramChatID)
	if mattermostURL != "" {
		slog.Info("mattermost fallback enabled")
	}

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

func (m *Monitor) checkAllServices(ctx context.Context) {
	var wg sync.WaitGroup
	startTime := time.Now()
	for _, ep := range m.config.Endpoints {
		wg.Add(1)
		go func(endpoint Endpoint) {
			defer wg.Done()
			m.checkService(ctx, endpoint)
		}(ep)
	}
	wg.Wait()
	duration := time.Since(startTime)
	slog.Info("health check cycle completed", "duration", duration.Round(time.Millisecond))
}

func (m *Monitor) checkService(ctx context.Context, ep Endpoint) {
	url := ep.URL
	success := m.performCheck(ctx, ep)

	if !success {
		time.Sleep(m.monitorConfig.retryDelay)
		success = m.performCheck(ctx, ep)
	}

	m.updateState(url, success)
}

func (m *Monitor) performCheck(ctx context.Context, ep Endpoint) bool {
	method := ep.Method
	if method == "" {
		method = "GET"
	}

	expectedStatus := ep.ExpectedStatus
	if expectedStatus == 0 {
		expectedStatus = 200
	}

	startTime := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, ep.URL, nil)
	if err != nil {
		slog.Error("failed to create request", "url", ep.URL, "error", err)
		return false
	}

	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		slog.Warn("request failed", "url", ep.URL, "error", err)
		return false
	}
	defer resp.Body.Close()

	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != expectedStatus {
		slog.Warn("unexpected status",
			"url", ep.URL,
			"status", resp.StatusCode,
			"expected", expectedStatus)
		return false
	}

	duration := time.Since(startTime)
	slog.Info("check successful",
		"url", ep.URL,
		"status", resp.StatusCode,
		"duration", duration.Round(time.Millisecond))
	return true
}

func (m *Monitor) updateState(url string, success bool) {
	m.statesMu.RLock()
	state := m.states[url]
	m.statesMu.RUnlock()

	state.mu.Lock()
	defer state.mu.Unlock()

	now := time.Now()
	state.lastCheckTime = now

	if success {
		if state.isDown {
			m.notifyQueue <- NotifyEvent{
				endpoint:  url,
				isDown:    false,
				timestamp: now,
				failTime:  state.firstFailTime,
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
			m.notifyQueue <- NotifyEvent{
				endpoint:  url,
				isDown:    true,
				timestamp: now,
				failTime:  state.firstFailTime,
			}
		}
	}
}

func (m *Monitor) notificationWorker(ctx context.Context) {
	defer m.wg.Done()

	var batch []NotifyEvent
	timer := time.NewTimer(m.monitorConfig.notifyBatchWindow)
	timer.Stop()

	for {
		select {
		case event, ok := <-m.notifyQueue:
			if !ok {
				if len(batch) > 0 {
					m.sendBatchNotification(batch)
				}
				return
			}
			batch = append(batch, event)
			if len(batch) == 1 {
				timer.Reset(m.monitorConfig.notifyBatchWindow)
			}
		case <-timer.C:
			if len(batch) > 0 {
				m.sendBatchNotification(batch)
				batch = nil
			}
		case <-ctx.Done():
			if len(batch) > 0 {
				m.sendBatchNotification(batch)
			}
			return
		}
	}
}

func (m *Monitor) sendBatchNotification(events []NotifyEvent) {
	if len(events) == 0 {
		return
	}

	var downServices, upServices []string
	downDetails := make(map[string]time.Time)
	upDetails := make(map[string]time.Time)

	for _, e := range events {
		if e.isDown {
			downServices = append(downServices, e.endpoint)
			downDetails[e.endpoint] = e.failTime
		} else {
			upServices = append(upServices, e.endpoint)
			upDetails[e.endpoint] = e.failTime
		}
	}

	var msg string
	if len(downServices) > 0 {
		msg += "🔴 *Services DOWN:*\n"
		for _, svc := range downServices {
			msg += fmt.Sprintf("• %s\n  Failed at: %s\n",
				svc, downDetails[svc].Format("2006-01-02 15:04:05"))
		}
	}

	if len(upServices) > 0 {
		if msg != "" {
			msg += "\n"
		}
		msg += "✅ *Services RECOVERED:*\n"
		for _, svc := range upServices {
			failTime := upDetails[svc]
			duration := time.Since(failTime).Round(time.Second)
			msg += fmt.Sprintf("• %s\n  Downtime: %s\n", svc, duration)
		}
	}

	if !m.sendToTelegram(msg) {
		if m.mattermostURL != "" {
			m.sendToMattermost(msg)
		} else {
			slog.Error("failed to send notifications, no fallback configured")
		}
	}
}

func (m *Monitor) sendToTelegram(message string) bool {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", m.telegramToken)

	payload := map[string]interface{}{
		"chat_id":    m.telegramChatID,
		"text":       message,
		"parse_mode": "Markdown",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal telegram payload", "error", err)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		slog.Error("failed to create telegram request", "error", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		slog.Warn("telegram request failed", "error", err)
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != 200 {
		slog.Warn("telegram returned non-200 status", "status", resp.StatusCode)
		return false
	}

	slog.Info("notification sent to Telegram")
	return true
}

func (m *Monitor) sendToMattermost(message string) {
	payload := map[string]string{
		"text": message,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		slog.Error("failed to marshal mattermost payload", "error", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", m.mattermostURL, bytes.NewReader(body))
	if err != nil {
		slog.Error("failed to create mattermost request", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		slog.Error("mattermost request failed", "error", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != 200 {
		slog.Error("mattermost returned non-200 status", "status", resp.StatusCode)
		return
	}

	slog.Info("notification sent to Mattermost (fallback)")
}
