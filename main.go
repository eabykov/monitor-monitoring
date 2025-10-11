package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
		log.Printf("WARN: invalid duration for %s=%s, using default %v", key, val, defaultVal)
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		var i int
		if _, err := fmt.Sscanf(val, "%d", &i); err == nil && i > 0 {
			return i
		}
		log.Printf("WARN: invalid integer for %s=%s, using default %d", key, val, defaultVal)
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
	if err := run(); err != nil {
		log.Fatalf("FATAL: %v", err)
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

	log.Printf("INFO: Loaded configuration with %d endpoints", len(config.Endpoints))
	for _, ep := range config.Endpoints {
		log.Printf("INFO: Monitoring %s [%s, expected: %d]", 
			ep.URL, 
			func() string { if ep.Method != "" { return ep.Method }; return "GET" }(),
			func() int { if ep.ExpectedStatus != 0 { return ep.ExpectedStatus }; return 200 }())
	}

	monitorConfig := loadMonitorConfig()
	log.Printf("INFO: Monitor configuration:")
	log.Printf("  Check interval: %v", monitorConfig.checkInterval)
	log.Printf("  Request timeout: %v", monitorConfig.requestTimeout)
	log.Printf("  Retry delay: %v", monitorConfig.retryDelay)
	log.Printf("  Failure threshold: %d", monitorConfig.failureThreshold)
	log.Printf("  Notify batch window: %v", monitorConfig.notifyBatchWindow)

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	monitor.wg.Add(1)
	go monitor.notificationWorker(ctx)

	log.Printf("INFO: Starting service monitor, checking every %v", monitorConfig.checkInterval)
	log.Printf("INFO: Telegram notifications enabled for chat %s", telegramChatID)
	if mattermostURL != "" {
		log.Printf("INFO: Mattermost fallback enabled")
	}

	ticker := time.NewTicker(monitorConfig.checkInterval)
	defer ticker.Stop()

	log.Println("INFO: Running initial health check...")
	monitor.checkAllServices(ctx)
	log.Println("INFO: Initial check completed")

	for {
		select {
		case <-ticker.C:
			monitor.checkAllServices(ctx)
		case <-sigChan:
			log.Println("INFO: Received shutdown signal, stopping gracefully...")
			cancel()
			monitor.wg.Wait()
			log.Println("INFO: Shutdown complete")
			return nil
		case <-ctx.Done():
			monitor.wg.Wait()
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
	log.Printf("INFO: Health check cycle completed in %v", duration.Round(time.Millisecond))
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
		log.Printf("ERROR: failed to create request for %s: %v", ep.URL, err)
		return false
	}

	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		log.Printf("WARN: request failed for %s: %v", ep.URL, err)
		return false
	}
	defer resp.Body.Close()

	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != expectedStatus {
		log.Printf("WARN: unexpected status %d (expected %d) for %s", 
			resp.StatusCode, expectedStatus, ep.URL)
		return false
	}

	duration := time.Since(startTime)
	log.Printf("INFO: ✓ %s [%d] %v", ep.URL, resp.StatusCode, duration.Round(time.Millisecond))
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
		case event := <-m.notifyQueue:
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
			log.Printf("ERROR: failed to send notifications, no fallback configured")
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
		log.Printf("ERROR: failed to marshal telegram payload: %v", err)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("ERROR: failed to create telegram request: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		log.Printf("WARN: telegram request failed: %v", err)
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != 200 {
		log.Printf("WARN: telegram returned status %d", resp.StatusCode)
		return false
	}

	log.Println("INFO: Notification sent to Telegram")
	return true
}

func (m *Monitor) sendToMattermost(message string) {
	payload := map[string]string{
		"text": message,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("ERROR: failed to marshal mattermost payload: %v", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", m.mattermostURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("ERROR: failed to create mattermost request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		log.Printf("ERROR: mattermost request failed: %v", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != 200 {
		log.Printf("ERROR: mattermost returned status %d", resp.StatusCode)
		return
	}

	log.Println("INFO: Notification sent to Mattermost (fallback)")
}
