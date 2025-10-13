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
		checkInterval:       getEnvDuration("CHECK_INTERVAL", 60*time.Second),
		requestTimeout:      getEnvDuration("REQUEST_TIMEOUT", 20*time.Second),
		retryDelay:          getEnvDuration("RETRY_DELAY", 10*time.Second),
		failureThreshold:    getEnvInt("FAILURE_THRESHOLD", 2),
		notifyBatchWindow:   getEnvDuration("NOTIFY_BATCH_WINDOW", 45*time.Second),
		maxBatchSize:        getEnvInt("MAX_BATCH_SIZE", 200),
		maxConcurrentChecks: getEnvInt("MAX_CONCURRENT_CHECKS", 32),
		maxResponseBodySize: int64(getEnvInt("MAX_RESPONSE_BODY_SIZE", 2097152)),
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

func getHostname() string {
	if hostname := os.Getenv("MONITOR_HOSTNAME"); hostname != "" {
		return strings.TrimSpace(hostname)
	}

	hostname, err := os.Hostname()
	if err != nil {
		slog.Warn("failed to get system hostname, using fallback", "error", err)
		return "unknown-host"
	}

	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return "unknown-host"
	}

	return hostname
}

type Config struct {
	Endpoints []Endpoint `yaml:"endpoints"`
}

type Endpoint struct {
	URL            string            `yaml:"url,omitempty"`
	Type           string            `yaml:"type,omitempty"`
	Method         string            `yaml:"method,omitempty"`
	ExpectedStatus int               `yaml:"expected_status,omitempty"`
	Headers        map[string]string `yaml:"headers,omitempty"`
	Host           string            `yaml:"host,omitempty"`
	RecordType     string            `yaml:"record_type,omitempty"`
	Expected       string            `yaml:"expected,omitempty"`
	Port           int               `yaml:"port,omitempty"`
	Address        string            `yaml:"address,omitempty"`
}

func (e Endpoint) GetIdentifier() string {
	switch strings.ToLower(e.Type) {
	case "dns":
		return fmt.Sprintf("dns://%s/%s", e.Host, e.RecordType)
	case "tcp":
		if e.Address != "" {
			return fmt.Sprintf("tcp://%s", e.Address)
		}
		return fmt.Sprintf("tcp://%s:%d", e.Host, e.Port)
	default:
		return e.URL
	}
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
	states         sync.Map
	telegramToken  string
	telegramChatID string
	mattermostURL  string
	httpClient     *http.Client
	resolver       *net.Resolver
	notifyQueue    chan *NotifyEvent
	semaphore      chan struct{}
	wg             sync.WaitGroup
	shutdown       chan struct{}
	hostname       string
}

type NotifyEvent struct {
	endpoint  string
	isDown    bool
	timestamp time.Time
	failTime  time.Time
}

type Notifier interface {
	Send(ctx context.Context, message string) error
	Name() string
}

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

	limited := io.LimitReader(resp.Body, 1<<20)
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return fmt.Errorf("drain body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

func maskSensitiveString(s string, showLast int) string {
	if len(s) <= showLast {
		return "***"
	}
	return strings.Repeat("*", len(s)-showLast) + s[len(s)-showLast:]
}

func sanitizeURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "[invalid-url]"
	}
	parsed.RawQuery = ""
	parsed.User = nil
	return parsed.String()
}

func validateEndpoint(ep *Endpoint) error {
	endpointType := strings.ToLower(ep.Type)
	if endpointType == "" {
		endpointType = "http"
		ep.Type = endpointType
	} else {
		ep.Type = endpointType
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
		ep.RecordType = recordType

		validTypes := map[string]bool{"A": true, "AAAA": true, "CNAME": true}
		if !validTypes[recordType] {
			return fmt.Errorf("unsupported DNS record type: %s (supported: A, AAAA, CNAME)", recordType)
		}

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
				ep.Expected = strings.TrimSuffix(ep.Expected, ".")
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

	if !strings.Contains(telegramToken, ":") || len(telegramToken) < 20 {
		return errors.New("TELEGRAM_BOT_TOKEN appears to be invalid")
	}

	if mattermostURL != "" {
		parsed, err := url.Parse(mattermostURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("MATTERMOST_WEBHOOK_URL is invalid: %w", err)
		}
	}

	hostname := getHostname()

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

	for i := range config.Endpoints {
		if err := validateEndpoint(&config.Endpoints[i]); err != nil {
			return fmt.Errorf("endpoint %d validation failed: %w", i, err)
		}
	}

	slog.Info("loaded configuration", "endpoints", len(config.Endpoints))
	for _, ep := range config.Endpoints {
		logEndpoint(ep)
	}

	monitorConfig := loadMonitorConfig()

	if monitorConfig.checkInterval < time.Second {
		return errors.New("CHECK_INTERVAL must be at least 1 second")
	}
	if monitorConfig.failureThreshold < 1 {
		return errors.New("FAILURE_THRESHOLD must be at least 1")
	}
	if monitorConfig.maxConcurrentChecks < 1 {
		return errors.New("MAX_CONCURRENT_CHECKS must be at least 1")
	}

	slog.Info("monitor configuration",
		"hostname", hostname,
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

	transport := &http.Transport{
		MaxIdleConns:          20,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		MaxConnsPerHost:       5,
		DisableKeepAlives:     false,
		DisableCompression:    false,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   monitorConfig.tcpTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	httpClient := &http.Client{
		Timeout:   monitorConfig.requestTimeout,
		Transport: transport,
	}

	resolver := &net.Resolver{
		PreferGo:     true,
		StrictErrors: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: monitorConfig.dnsTimeout,
			}
			return d.DialContext(ctx, network, address)
		},
	}

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
		shutdown:       make(chan struct{}),
		hostname:       hostname,
	}

	for _, ep := range config.Endpoints {
		monitor.states.Store(ep.GetIdentifier(), &ServiceState{})
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	monitor.wg.Add(1)
	go monitor.notificationWorker(ctx, notifiers)

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

			close(monitor.shutdown)

			ticker.Stop()

			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer shutdownCancel()

			done := make(chan struct{})
			go func() {
				close(monitor.notifyQueue)
				monitor.wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				slog.Info("shutdown complete")
			case <-shutdownCtx.Done():
				slog.Warn("shutdown timeout, forcing exit")
			}

			transport.CloseIdleConnections()

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
	default:
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

func (m *Monitor) memoryMonitor(ctx context.Context) {
	defer m.wg.Done()

	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			var memStats runtime.MemStats
			runtime.ReadMemStats(&memStats)

			slog.Info("memory stats",
				"alloc_mb", memStats.Alloc/1024/1024,
				"sys_mb", memStats.Sys/1024/1024,
				"num_gc", memStats.NumGC,
				"goroutines", runtime.NumGoroutine())

			if memStats.Alloc > 500*1024*1024 {
				slog.Warn("high memory usage detected", "alloc_mb", memStats.Alloc/1024/1024)
				runtime.GC()
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
		select {
		case <-m.shutdown:
			slog.Debug("shutdown signal received, stopping health checks")
			wg.Wait()
			return
		default:
		}

		wg.Add(1)
		go func(endpoint Endpoint) {
			defer wg.Done()

			select {
			case m.semaphore <- struct{}{}:
				defer func() { <-m.semaphore }()
				m.checkService(ctx, endpoint)
			case <-ctx.Done():
				return
			case <-m.shutdown:
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
		select {
		case <-m.shutdown:
			return
		default:
		}

		retryCtx, cancel := context.WithTimeout(ctx, m.monitorConfig.retryDelay+m.monitorConfig.requestTimeout)
		defer cancel()

		time.Sleep(m.monitorConfig.retryDelay)

		if retryCtx.Err() == nil {
			success = m.performCheck(retryCtx, ep)
		}
	}

	m.updateState(identifier, success)
}

func (m *Monitor) performCheck(ctx context.Context, ep Endpoint) bool {
	switch strings.ToLower(ep.Type) {
	case "dns":
		return m.performDNSCheck(ctx, ep)
	case "tcp":
		return m.performTCPCheck(ctx, ep)
	default:
		return m.performHTTPCheck(ctx, ep)
	}
}

type dnsLookupResult struct {
	records   []string
	recordStr string
	err       error
}

func (m *Monitor) lookupIPv4(ctx context.Context, host string) dnsLookupResult {
	ips, err := m.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return dnsLookupResult{err: err}
	}

	var ipv4s []string
	for _, ip := range ips {
		if ip.IP.To4() != nil {
			ipv4s = append(ipv4s, ip.IP.String())
		}
	}

	if len(ipv4s) == 0 {
		return dnsLookupResult{err: errors.New("no IPv4 addresses found")}
	}

	return dnsLookupResult{
		records:   ipv4s,
		recordStr: strings.Join(ipv4s, ", "),
	}
}

func (m *Monitor) lookupIPv6(ctx context.Context, host string) dnsLookupResult {
	ips, err := m.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return dnsLookupResult{err: err}
	}

	var ipv6s []string
	for _, ip := range ips {
		if ip.IP.To4() == nil && ip.IP.To16() != nil {
			ipv6s = append(ipv6s, ip.IP.String())
		}
	}

	if len(ipv6s) == 0 {
		return dnsLookupResult{err: errors.New("no IPv6 addresses found")}
	}

	return dnsLookupResult{
		records:   ipv6s,
		recordStr: strings.Join(ipv6s, ", "),
	}
}

func (m *Monitor) lookupCNAME(ctx context.Context, host string) dnsLookupResult {
	cname, err := m.resolver.LookupCNAME(ctx, host)
	if err != nil {
		return dnsLookupResult{err: err}
	}

	cname = strings.TrimSuffix(cname, ".")
	return dnsLookupResult{
		records:   []string{cname},
		recordStr: cname,
	}
}

func validateDNSResult(result dnsLookupResult, expected string) bool {
	if expected == "" {
		return true
	}

	expected = strings.TrimSuffix(expected, ".")
	for _, record := range result.records {
		if record == expected {
			return true
		}
	}
	return false
}

func (m *Monitor) performDNSCheck(ctx context.Context, ep Endpoint) bool {
	recordType := strings.ToUpper(ep.RecordType)
	startTime := time.Now()

	dnsCtx, cancel := context.WithTimeout(ctx, m.monitorConfig.dnsTimeout)
	defer cancel()

	var result dnsLookupResult

	switch recordType {
	case "A":
		result = m.lookupIPv4(dnsCtx, ep.Host)
	case "AAAA":
		result = m.lookupIPv6(dnsCtx, ep.Host)
	case "CNAME":
		result = m.lookupCNAME(dnsCtx, ep.Host)
	default:
		slog.Error("unsupported DNS record type", "type", recordType)
		return false
	}

	if result.err != nil {
		slog.Warn("DNS lookup failed",
			"host", ep.Host,
			"type", recordType,
			"error", result.err)
		return false
	}

	if !validateDNSResult(result, ep.Expected) {
		slog.Warn("DNS record mismatch",
			"host", ep.Host,
			"type", recordType,
			"expected", ep.Expected,
			"got", result.recordStr)
		return false
	}

	duration := time.Since(startTime)
	slog.Debug("DNS check successful",
		"host", ep.Host,
		"type", recordType,
		"result", result.recordStr,
		"duration", duration.Round(time.Millisecond))

	return true
}

func (m *Monitor) performTCPCheck(ctx context.Context, ep Endpoint) bool {
	address := ep.Address
	if address == "" {
		address = fmt.Sprintf("%s:%d", ep.Host, ep.Port)
	}

	startTime := time.Now()

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
	defer func() {
		if cerr := conn.Close(); cerr != nil {
			slog.Debug("failed to close TCP connection",
				"address", address,
				"error", cerr)
		}
	}()

	duration := time.Since(startTime)
	slog.Debug("TCP check successful",
		"address", address,
		"duration", duration.Round(time.Millisecond))

	return true
}

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

	limited := io.LimitReader(resp.Body, m.monitorConfig.maxResponseBodySize)
	written, err := io.Copy(io.Discard, limited)
	if err != nil {
		slog.Warn("failed to drain response body",
			"url", sanitizeURL(ep.URL),
			"error", err)
	}

	if written >= m.monitorConfig.maxResponseBodySize {
		slog.Debug("response body truncated",
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
		slog.Error("state not found for identifier", "id", identifier)
		return
	}
	state := val.(*ServiceState)

	state.mu.Lock()
	defer state.mu.Unlock()

	now := time.Now()
	state.lastCheckTime = now

	if success {
		wasDown := state.isDown
		if wasDown {
			event := &NotifyEvent{
				endpoint:  identifier,
				isDown:    false,
				timestamp: now,
				failTime:  state.firstFailTime,
			}

			select {
			case m.notifyQueue <- event:
			case <-m.shutdown:
			default:
				slog.Warn("notification queue full, dropping recovery event",
					"id", identifier)
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

			event := &NotifyEvent{
				endpoint:  identifier,
				isDown:    true,
				timestamp: now,
				failTime:  state.firstFailTime,
			}

			select {
			case m.notifyQueue <- event:
			case <-m.shutdown:
			default:
				slog.Warn("notification queue full, dropping failure event",
					"id", identifier)
			}
		}
	}
}

func (m *Monitor) notificationWorker(ctx context.Context, notifiers []Notifier) {
	defer m.wg.Done()

	batch := make([]*NotifyEvent, 0, m.monitorConfig.maxBatchSize)
	timer := time.NewTimer(m.monitorConfig.notifyBatchWindow)
	timer.Stop()

	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()

	flush := func() {
		if len(batch) == 0 {
			return
		}

		eventsCopy := make([]NotifyEvent, len(batch))
		for i, event := range batch {
			eventsCopy[i] = *event
		}

		m.sendBatchNotification(ctx, eventsCopy, notifiers)

		batch = batch[:0]
	}

	for {
		select {
		case event, ok := <-m.notifyQueue:
			if !ok {
				flush()
				return
			}

			batch = append(batch, event)

			if len(batch) >= m.monitorConfig.maxBatchSize {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				flush()
			} else if len(batch) == 1 {
				timer.Reset(m.monitorConfig.notifyBatchWindow)
			}

		case <-timer.C:
			flush()

		case <-ctx.Done():
			flush()
			return
		}
	}
}

func (m *Monitor) sendBatchNotification(ctx context.Context, events []NotifyEvent, notifiers []Notifier) {
	if len(events) == 0 {
		return
	}

	downServices := make([]string, 0, len(events))
	upServices := make([]string, 0, len(events))
	downDetails := make(map[string]time.Time, len(events))
	upDetails := make(map[string]time.Time, len(events))

	for i := range events {
		e := &events[i]
		if e.isDown {
			downServices = append(downServices, e.endpoint)
			downDetails[e.endpoint] = e.failTime
		} else {
			upServices = append(upServices, e.endpoint)
			upDetails[e.endpoint] = e.failTime
		}
	}

	estimatedSize := 150 + len(m.hostname)
	if len(downServices) > 0 {
		for _, svc := range downServices {
			estimatedSize += len(svc) + 60
		}
	}
	if len(upServices) > 0 {
		for _, svc := range upServices {
			estimatedSize += len(svc) + 70
		}
	}

	var sb strings.Builder
	sb.Grow(estimatedSize)

	sb.WriteString("🖥 *Host:* `")
	sb.WriteString(m.hostname)
	sb.WriteString("`\n\n")

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
		if sb.Len() > len(m.hostname)+20 {
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

	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var lastErr error
	for _, notifier := range notifiers {
		if err := notifier.Send(notifyCtx, msg); err != nil {
			lastErr = err
			slog.Warn("notification failed",
				"notifier", notifier.Name(),
				"error", err)
			continue
		}
		slog.Info("notification sent", "notifier", notifier.Name())
		return
	}

	if lastErr != nil {
		slog.Error("failed to send notification through all channels", "last_error", lastErr)
	}
}
