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
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	CheckInterval       time.Duration `env:"CHECK_INTERVAL" default:"60s"`
	RequestTimeout      time.Duration `env:"REQUEST_TIMEOUT" default:"10s"`
	RetryDelay          time.Duration `env:"RETRY_DELAY" default:"3s"`
	FailureThreshold    int           `env:"FAILURE_THRESHOLD" default:"2"`
	NotifyBatchWindow   time.Duration `env:"NOTIFY_BATCH_WINDOW" default:"40s"`
	MaxBatchSize        int           `env:"MAX_BATCH_SIZE" default:"50"`
	MaxConcurrentChecks int           `env:"MAX_CONCURRENT_CHECKS" default:"20"`
	MaxResponseBodySize int64         `env:"MAX_RESPONSE_BODY_SIZE" default:"524288"`
	DNSTimeout          time.Duration `env:"DNS_TIMEOUT" default:"5s"`
	TCPTimeout          time.Duration `env:"TCP_TIMEOUT" default:"8s"`
	Hostname            string        `env:"MONITOR_HOSTNAME"`
	ConfigPath          string        `env:"CONFIG_PATH" default:"config.yaml"`

	PrimaryNotifier  string `env:"PRIMARY_NOTIFIER" default:"telegram"`
	FallbackNotifier string `env:"FALLBACK_NOTIFIER" default:""`

	TelegramToken  string `env:"TELEGRAM_BOT_TOKEN"`
	TelegramChatID string `env:"TELEGRAM_CHAT_ID"`
	MattermostURL  string `env:"MATTERMOST_WEBHOOK_URL"`
	SlackURL       string `env:"SLACK_WEBHOOK_URL"`
	DiscordURL     string `env:"DISCORD_WEBHOOK_URL"`

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

type WebhookNotifier struct {
	name       string
	url        string
	httpClient *http.Client
	formatFunc func(string) map[string]interface{}
}

func (w *WebhookNotifier) Send(ctx context.Context, message string) error {
	payload := w.formatFunc(message)
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close response body", "error", closeErr)
		}
	}()

	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
		slog.Warn("failed to drain response body", "error", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func (w *WebhookNotifier) Name() string { return w.name }

type Monitor struct {
	config      Config
	states      sync.Map
	httpClient  *http.Client
	resolver    *net.Resolver
	notifyQueue chan *NotifyEvent
	semaphore   chan struct{}
	wg          sync.WaitGroup
	shutdown    chan struct{}
}

func loadConfig() (*Config, error) {
	cfg := &Config{}

	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		envKey := field.Tag.Get("env")
		defaultVal := field.Tag.Get("default")

		if envKey == "" {
			continue
		}

		envVal := os.Getenv(envKey)
		if envVal == "" {
			envVal = defaultVal
		}
		if envVal == "" {
			continue
		}

		fieldVal := v.Field(i)
		switch fieldVal.Kind() {
		case reflect.String:
			fieldVal.SetString(envVal)
		case reflect.Int, reflect.Int64:
			if field.Type == reflect.TypeOf(time.Duration(0)) {
				if d, err := time.ParseDuration(envVal); err == nil {
					fieldVal.SetInt(int64(d))
				}
			} else {
				var n int
				if _, err := fmt.Sscanf(envVal, "%d", &n); err == nil && n > 0 {
					fieldVal.SetInt(int64(n))
				}
			}
		}
	}

	if cfg.Hostname == "" {
		if h, err := os.Hostname(); err == nil {
			cfg.Hostname = strings.TrimSpace(h)
		}
		if cfg.Hostname == "" {
			cfg.Hostname = "unknown-host"
		}
	}

	data, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		return nil, err
	}

	var yamlCfg struct {
		Endpoints []Endpoint `yaml:"endpoints"`
	}
	if err := yaml.Unmarshal(data, &yamlCfg); err != nil {
		return nil, err
	}
	cfg.Endpoints = yamlCfg.Endpoints

	return cfg, nil
}

func validateEndpoint(ep *Endpoint) error {
	if ep.Type == "" {
		ep.Type = "http"
	}
	ep.Type = strings.ToLower(ep.Type)

	switch ep.Type {
	case "http":
		if ep.URL == "" {
			return errors.New("URL required")
		}
		parsed, err := url.Parse(ep.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return errors.New("invalid URL")
		}
		if ep.Method != "" {
			ep.Method = strings.ToUpper(ep.Method)
		}

	case "dns":
		if ep.Host == "" || ep.RecordType == "" {
			return errors.New("host and record_type required")
		}
		ep.RecordType = strings.ToUpper(ep.RecordType)
		if ep.RecordType != "A" && ep.RecordType != "AAAA" && ep.RecordType != "CNAME" {
			return errors.New("unsupported DNS type")
		}
		ep.Expected = strings.TrimSuffix(ep.Expected, ".")

	case "tcp":
		if ep.Address == "" {
			if ep.Host == "" || ep.Port <= 0 || ep.Port > 65535 {
				return errors.New("valid address or host:port required")
			}
			ep.Address = fmt.Sprintf("%s:%d", ep.Host, ep.Port)
		}

	default:
		return errors.New("unsupported type")
	}
	return nil
}

func createNotifiers(cfg *Config, client *http.Client) ([]Notifier, error) {
	notifierMap := map[string]func() (Notifier, error){
		"telegram": func() (Notifier, error) {
			if cfg.TelegramToken == "" || cfg.TelegramChatID == "" {
				return nil, errors.New("telegram credentials missing")
			}
			return &WebhookNotifier{
				name:       "Telegram",
				url:        fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.TelegramToken),
				httpClient: client,
				formatFunc: func(msg string) map[string]interface{} {
					return map[string]interface{}{
						"chat_id":              cfg.TelegramChatID,
						"text":                 msg,
						"parse_mode":           "Markdown",
						"link_preview_options": map[string]bool{"is_disabled": true},
					}
				},
			}, nil
		},
		"mattermost": func() (Notifier, error) {
			if cfg.MattermostURL == "" {
				return nil, errors.New("mattermost URL missing")
			}
			return &WebhookNotifier{
				name:       "Mattermost",
				url:        cfg.MattermostURL,
				httpClient: client,
				formatFunc: func(msg string) map[string]interface{} {
					return map[string]interface{}{"text": msg}
				},
			}, nil
		},
		"slack": func() (Notifier, error) {
			if cfg.SlackURL == "" {
				return nil, errors.New("slack URL missing")
			}
			return &WebhookNotifier{
				name:       "Slack",
				url:        cfg.SlackURL,
				httpClient: client,
				formatFunc: func(msg string) map[string]interface{} {
					// Slack использует тот же markdown формат
					return map[string]interface{}{"text": msg, "mrkdwn": true}
				},
			}, nil
		},
		"discord": func() (Notifier, error) {
			if cfg.DiscordURL == "" {
				return nil, errors.New("discord URL missing")
			}
			return &WebhookNotifier{
				name:       "Discord",
				url:        cfg.DiscordURL,
				httpClient: client,
				formatFunc: func(msg string) map[string]interface{} {
					return map[string]interface{}{"content": strings.ReplaceAll(msg, "*", "**")}
				},
			}, nil
		},
	}

	var notifiers []Notifier
	for _, name := range []string{cfg.PrimaryNotifier, cfg.FallbackNotifier} {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}

		createFunc, ok := notifierMap[name]
		if !ok {
			slog.Warn("unknown notifier", "name", name)
			continue
		}

		notifier, err := createFunc()
		if err != nil {
			slog.Warn("failed to create notifier", "name", name, "error", err)
			continue
		}

		notifiers = append(notifiers, notifier)
		slog.Info("notifier configured", "name", name, "position",
			map[bool]string{true: "primary", false: "fallback"}[len(notifiers) == 1])
	}

	if len(notifiers) == 0 {
		return nil, errors.New("no valid notifiers configured")
	}

	return notifiers, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	if len(cfg.Endpoints) == 0 {
		slog.Error("no endpoints configured")
		os.Exit(1)
	}

	for i := range cfg.Endpoints {
		if err := validateEndpoint(&cfg.Endpoints[i]); err != nil {
			slog.Error("endpoint validation failed", "index", i, "error", err)
			os.Exit(1)
		}
	}

	slog.Info("configuration loaded",
		"endpoints", len(cfg.Endpoints),
		"hostname", cfg.Hostname,
		"primary_notifier", cfg.PrimaryNotifier,
		"fallback_notifier", cfg.FallbackNotifier)

	transport := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     30 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   cfg.TCPTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	httpClient := &http.Client{
		Timeout:   cfg.RequestTimeout,
		Transport: transport,
	}

	notifiers, err := createNotifiers(cfg, httpClient)
	if err != nil {
		slog.Error("notifier setup failed", "error", err)
		os.Exit(1)
	}

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{Timeout: cfg.DNSTimeout}).DialContext(ctx, network, address)
		},
	}

	monitor := &Monitor{
		config:      *cfg,
		httpClient:  httpClient,
		resolver:    resolver,
		notifyQueue: make(chan *NotifyEvent, 100),
		semaphore:   make(chan struct{}, cfg.MaxConcurrentChecks),
		shutdown:    make(chan struct{}),
	}

	for _, ep := range cfg.Endpoints {
		monitor.states.Store(ep.GetIdentifier(), &ServiceState{})
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	monitor.wg.Add(1)
	go monitor.notificationWorker(ctx, notifiers)

	ticker := time.NewTicker(cfg.CheckInterval)
	defer ticker.Stop()

	monitor.checkAllServices(ctx)

	for {
		select {
		case <-ticker.C:
			monitor.checkAllServices(ctx)
		case <-ctx.Done():
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
				slog.Warn("shutdown timeout")
			}

			transport.CloseIdleConnections()
			return
		}
	}
}

func (m *Monitor) checkAllServices(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	var wg sync.WaitGroup
	for _, ep := range m.config.Endpoints {
		select {
		case <-m.shutdown:
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
			case <-m.shutdown:
			}
		}(ep)
	}
	wg.Wait()
}

func (m *Monitor) checkService(ctx context.Context, ep Endpoint) {
	if ctx.Err() != nil {
		return
	}

	success := m.performCheck(ctx, ep)
	if !success && ctx.Err() == nil {
		select {
		case <-m.shutdown:
			return
		default:
		}
		time.Sleep(m.config.RetryDelay)
		success = m.performCheck(ctx, ep)
	}

	m.updateState(ep.GetIdentifier(), success)
}

func (m *Monitor) performCheck(ctx context.Context, ep Endpoint) bool {
	switch ep.Type {
	case "dns":
		return m.checkDNS(ctx, ep)
	case "tcp":
		return m.checkTCP(ctx, ep)
	default:
		return m.checkHTTP(ctx, ep)
	}
}

func (m *Monitor) checkDNS(ctx context.Context, ep Endpoint) bool {
	dnsCtx, cancel := context.WithTimeout(ctx, m.config.DNSTimeout)
	defer cancel()

	var records []string

	if ep.RecordType == "CNAME" {
		cname, err := m.resolver.LookupCNAME(dnsCtx, ep.Host)
		if err != nil {
			return false
		}
		records = []string{strings.TrimSuffix(cname, ".")}
	} else {
		ips, err := m.resolver.LookupIPAddr(dnsCtx, ep.Host)
		if err != nil {
			return false
		}
		for _, ip := range ips {
			if ep.RecordType == "A" && ip.IP.To4() != nil {
				records = append(records, ip.IP.String())
			} else if ep.RecordType == "AAAA" && ip.IP.To4() == nil && ip.IP.To16() != nil {
				records = append(records, ip.IP.String())
			}
		}
		if len(records) == 0 {
			return false
		}
	}

	if ep.Expected != "" {
		for _, record := range records {
			if record == ep.Expected {
				return true
			}
		}
		return false
	}

	return true
}

func (m *Monitor) checkTCP(ctx context.Context, ep Endpoint) bool {
	conn, err := (&net.Dialer{Timeout: m.config.TCPTimeout}).DialContext(ctx, "tcp", ep.Address)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (m *Monitor) checkHTTP(ctx context.Context, ep Endpoint) bool {
	method := ep.Method
	if method == "" {
		method = http.MethodGet
	}

	expectedStatus := ep.ExpectedStatus
	if expectedStatus == 0 {
		expectedStatus = http.StatusOK
	}

	req, err := http.NewRequestWithContext(ctx, method, ep.URL, nil)
	if err != nil {
		return false
	}

	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close response body", "error", closeErr)
		}
	}()

	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, m.config.MaxResponseBodySize)); err != nil {
		slog.Warn("failed to drain response body", "error", err)
	}

	return resp.StatusCode == expectedStatus
}

func (m *Monitor) updateState(identifier string, success bool) {
	val, ok := m.states.Load(identifier)
	if !ok {
		return
	}
	state := val.(*ServiceState)

	state.mu.Lock()
	defer state.mu.Unlock()

	now := time.Now()

	if success {
		if state.isDown {
			select {
			case m.notifyQueue <- &NotifyEvent{
				endpoint:  identifier,
				isDown:    false,
				timestamp: now,
				failTime:  state.firstFailTime,
			}:
			case <-m.shutdown:
			default:
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

		if state.consecutiveFails >= m.config.FailureThreshold && !state.isDown {
			state.isDown = true
			select {
			case m.notifyQueue <- &NotifyEvent{
				endpoint:  identifier,
				isDown:    true,
				timestamp: now,
				failTime:  state.firstFailTime,
			}:
			case <-m.shutdown:
			default:
			}
		}
	}
}

func (m *Monitor) notificationWorker(ctx context.Context, notifiers []Notifier) {
	defer m.wg.Done()

	var batch []*NotifyEvent
	timer := time.NewTimer(m.config.NotifyBatchWindow)
	timer.Stop()

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
			if len(batch) >= m.config.MaxBatchSize {
				timer.Stop()
				flush()
			} else if len(batch) == 1 {
				timer.Reset(m.config.NotifyBatchWindow)
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

	var sb strings.Builder
	sb.WriteString("*Host:* `")
	sb.WriteString(m.config.Hostname)
	sb.WriteString("`\n\n")

	var downServices, upServices []NotifyEvent
	for i := range events {
		if events[i].isDown {
			downServices = append(downServices, events[i])
		} else {
			upServices = append(upServices, events[i])
		}
	}

	if len(downServices) > 0 {
		sb.WriteString("*Services DOWN:*\n")
		for _, e := range downServices {
			sb.WriteString("- ")
			sb.WriteString(e.endpoint)
			sb.WriteString("\n  Failed at: ")
			sb.WriteString(e.failTime.Format("2006-01-02 15:04:05"))
			sb.WriteString("\n")
		}
	}

	if len(upServices) > 0 {
		if len(downServices) > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("*Services RECOVERED:*\n")
		for _, e := range upServices {
			duration := time.Since(e.failTime).Round(time.Second)
			sb.WriteString("- ")
			sb.WriteString(e.endpoint)
			sb.WriteString("\n  Downtime: ")
			sb.WriteString(duration.String())
			sb.WriteString("\n")
		}
	}

	msg := sb.String()
	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	for _, notifier := range notifiers {
		if err := notifier.Send(notifyCtx, msg); err != nil {
			slog.Warn("notification failed", "notifier", notifier.Name(), "error", err)
			continue
		}
		slog.Info("notification sent", "notifier", notifier.Name())
		return
	}

	slog.Error("all notifications failed")
}
