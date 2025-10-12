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
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
	"gopkg.in/yaml.v3"
)

// Version information
const (
	Version   = "2.0.0"
	UserAgent = "MonitorService/" + Version
)

// Configuration constants with sane defaults
const (
	DefaultCheckInterval       = 60 * time.Second
	DefaultRequestTimeout      = 30 * time.Second
	DefaultRetryDelay         = 5 * time.Second
	DefaultFailureThreshold   = 3
	DefaultNotifyBatchWindow  = 10 * time.Second
	DefaultMaxBatchSize       = 50
	DefaultMaxConcurrentChecks = 10
	DefaultMaxResponseBodySize = 1 << 20 // 1MB
	DefaultDNSTimeout         = 5 * time.Second
	DefaultTCPTimeout         = 10 * time.Second
	DefaultShutdownTimeout    = 30 * time.Second
)

// MonitorConfig holds all configuration for the monitor
type MonitorConfig struct {
	CheckInterval       time.Duration `json:"check_interval"`
	RequestTimeout      time.Duration `json:"request_timeout"`
	RetryDelay         time.Duration `json:"retry_delay"`
	FailureThreshold   int           `json:"failure_threshold"`
	NotifyBatchWindow  time.Duration `json:"notify_batch_window"`
	MaxBatchSize       int           `json:"max_batch_size"`
	MaxConcurrentChecks int          `json:"max_concurrent_checks"`
	MaxResponseBodySize int64        `json:"max_response_body_size"`
	DNSTimeout         time.Duration `json:"dns_timeout"`
	TCPTimeout         time.Duration `json:"tcp_timeout"`
	ShutdownTimeout    time.Duration `json:"shutdown_timeout"`
	EnableMetrics      bool          `json:"enable_metrics"`
	MetricsPort        int           `json:"metrics_port"`
}

// LoadMonitorConfig loads configuration from environment with validation
func LoadMonitorConfig() (*MonitorConfig, error) {
	cfg := &MonitorConfig{
		CheckInterval:       getEnvDuration("CHECK_INTERVAL", DefaultCheckInterval),
		RequestTimeout:      getEnvDuration("REQUEST_TIMEOUT", DefaultRequestTimeout),
		RetryDelay:         getEnvDuration("RETRY_DELAY", DefaultRetryDelay),
		FailureThreshold:   getEnvInt("FAILURE_THRESHOLD", DefaultFailureThreshold),
		NotifyBatchWindow:  getEnvDuration("NOTIFY_BATCH_WINDOW", DefaultNotifyBatchWindow),
		MaxBatchSize:       getEnvInt("MAX_BATCH_SIZE", DefaultMaxBatchSize),
		MaxConcurrentChecks: getEnvInt("MAX_CONCURRENT_CHECKS", DefaultMaxConcurrentChecks),
		MaxResponseBodySize: int64(getEnvInt("MAX_RESPONSE_BODY_SIZE", DefaultMaxResponseBodySize)),
		DNSTimeout:         getEnvDuration("DNS_TIMEOUT", DefaultDNSTimeout),
		TCPTimeout:         getEnvDuration("TCP_TIMEOUT", DefaultTCPTimeout),
		ShutdownTimeout:    getEnvDuration("SHUTDOWN_TIMEOUT", DefaultShutdownTimeout),
		EnableMetrics:      getEnvBool("ENABLE_METRICS", true),
		MetricsPort:        getEnvInt("METRICS_PORT", 9090),
	}

	// Validate configuration
	if cfg.CheckInterval < time.Second {
		return nil, errors.New("check_interval must be at least 1 second")
	}
	if cfg.MaxConcurrentChecks < 1 || cfg.MaxConcurrentChecks > 1000 {
		return nil, errors.New("max_concurrent_checks must be between 1 and 1000")
	}
	if cfg.FailureThreshold < 1 || cfg.FailureThreshold > 100 {
		return nil, errors.New("failure_threshold must be between 1 and 100")
	}

	return cfg, nil
}

// Config represents the service configuration
type Config struct {
	Endpoints []Endpoint `yaml:"endpoints" json:"endpoints"`
	Version   string     `yaml:"version" json:"version"`
}

// Endpoint represents a service endpoint to monitor
type Endpoint struct {
	URL            string            `yaml:"url,omitempty" json:"url,omitempty"`
	Type           string            `yaml:"type,omitempty" json:"type,omitempty"`
	Method         string            `yaml:"method,omitempty" json:"method,omitempty"`
	ExpectedStatus int               `yaml:"expected_status,omitempty" json:"expected_status,omitempty"`
	Headers        map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Host           string            `yaml:"host,omitempty" json:"host,omitempty"`
	RecordType     string            `yaml:"record_type,omitempty" json:"record_type,omitempty"`
	Expected       string            `yaml:"expected,omitempty" json:"expected,omitempty"`
	Port           int               `yaml:"port,omitempty" json:"port,omitempty"`
	Address        string            `yaml:"address,omitempty" json:"address,omitempty"`
	Timeout        time.Duration     `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Tags           []string          `yaml:"tags,omitempty" json:"tags,omitempty"`
}

// GetIdentifier returns a unique identifier for the endpoint
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

// ServiceState represents the current state of a monitored service
type ServiceState struct {
	mu                 sync.RWMutex
	consecutiveFails   int32
	isDown             atomic.Bool
	firstFailTime      atomic.Value // time.Time
	lastCheckTime      atomic.Value // time.Time
	lastResponseTime   atomic.Value // time.Duration
	totalChecks        atomic.Uint64
	totalFailures      atomic.Uint64
	lastError          atomic.Value // string
}

// NewServiceState creates a new service state
func NewServiceState() *ServiceState {
	s := &ServiceState{}
	s.firstFailTime.Store(time.Time{})
	s.lastCheckTime.Store(time.Time{})
	s.lastResponseTime.Store(time.Duration(0))
	s.lastError.Store("")
	return s
}

// Update updates the service state atomically
func (s *ServiceState) Update(success bool, responseTime time.Duration, err error) (stateChanged bool, isDown bool) {
	s.totalChecks.Add(1)
	s.lastCheckTime.Store(time.Now())
	s.lastResponseTime.Store(responseTime)
	
	if err != nil {
		s.lastError.Store(err.Error())
	} else {
		s.lastError.Store("")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if success {
		if s.isDown.Load() {
			s.isDown.Store(false)
			s.consecutiveFails = 0
			s.firstFailTime.Store(time.Time{})
			return true, false
		}
		s.consecutiveFails = 0
	} else {
		s.totalFailures.Add(1)
		oldFails := s.consecutiveFails
		s.consecutiveFails++
		
		if oldFails == 0 {
			s.firstFailTime.Store(time.Now())
		}
		
		// Check if we just crossed the threshold
		if s.consecutiveFails >= int32(DefaultFailureThreshold) && !s.isDown.Load() {
			s.isDown.Store(true)
			return true, true
		}
	}
	
	return false, s.isDown.Load()
}

// Checker interface for different check types
type Checker interface {
	Check(ctx context.Context, endpoint Endpoint) error
	Type() string
}

// HTTPChecker implements HTTP endpoint checking
type HTTPChecker struct {
	client              *http.Client
	maxResponseBodySize int64
	rateLimiter        *rate.Limiter
}

// NewHTTPChecker creates a new HTTP checker with optimized settings
func NewHTTPChecker(timeout time.Duration, maxBodySize int64) *HTTPChecker {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:          (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		MaxConnsPerHost:       20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    false,
		DisableKeepAlives:     false,
		ResponseHeaderTimeout: timeout,
	}

	return &HTTPChecker{
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
		maxResponseBodySize: maxBodySize,
		rateLimiter:        rate.NewLimiter(rate.Every(100*time.Millisecond), 10),
	}
}

// Check performs an HTTP check
func (h *HTTPChecker) Check(ctx context.Context, endpoint Endpoint) error {
	// Rate limiting
	if err := h.rateLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limit: %w", err)
	}

	method := endpoint.Method
	if method == "" {
		method = http.MethodGet
	}

	expectedStatus := endpoint.ExpectedStatus
	if expectedStatus == 0 {
		expectedStatus = http.StatusOK
	}

	// Validate URL to prevent SSRF
	if err := validateURL(endpoint.URL); err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint.URL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", UserAgent)
	for k, v := range endpoint.Headers {
		req.Header.Set(k, v)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()

	// Use io.Copy with limit reader for efficiency
	limited := io.LimitReader(resp.Body, h.maxResponseBodySize)
	written, err := io.Copy(io.Discard, limited)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if written >= h.maxResponseBodySize {
		slog.Warn("response body truncated",
			"url", sanitizeURL(endpoint.URL),
			"size", written)
	}

	if resp.StatusCode != expectedStatus {
		return fmt.Errorf("unexpected status code: got %d, want %d", resp.StatusCode, expectedStatus)
	}

	return nil
}

// Type returns the checker type
func (h *HTTPChecker) Type() string {
	return "http"
}

// DNSChecker implements DNS endpoint checking
type DNSChecker struct {
	resolver *net.Resolver
	cache    *DNSCache
}

// DNSCache implements a simple DNS cache with TTL
type DNSCache struct {
	mu      sync.RWMutex
	entries map[string]*dnsCacheEntry
}

type dnsCacheEntry struct {
	value     interface{}
	expiresAt time.Time
}

// NewDNSCache creates a new DNS cache
func NewDNSCache() *DNSCache {
	cache := &DNSCache{
		entries: make(map[string]*dnsCacheEntry),
	}
	
	// Start cleanup goroutine
	go cache.cleanup()
	
	return cache
}

func (c *DNSCache) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		c.mu.Lock()
		now := time.Now()
		for k, v := range c.entries {
			if v.expiresAt.Before(now) {
				delete(c.entries, k)
			}
		}
		c.mu.Unlock()
	}
}

func (c *DNSCache) Get(key string) (interface{}, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	entry, ok := c.entries[key]
	if !ok || entry.expiresAt.Before(time.Now()) {
		return nil, false
	}
	
	return entry.value, true
}

func (c *DNSCache) Set(key string, value interface{}, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	c.entries[key] = &dnsCacheEntry{
		value:     value,
		expiresAt: time.Now().Add(ttl),
	}
}

// NewDNSChecker creates a new DNS checker
func NewDNSChecker(timeout time.Duration) *DNSChecker {
	return &DNSChecker{
		resolver: &net.Resolver{
			PreferGo:     true,
			StrictErrors: true,
		},
		cache: NewDNSCache(),
	}
}

// Check performs a DNS check
func (d *DNSChecker) Check(ctx context.Context, endpoint Endpoint) error {
	recordType := strings.ToUpper(endpoint.RecordType)
	cacheKey := fmt.Sprintf("%s:%s", endpoint.Host, recordType)
	
	// Check cache first
	if cached, ok := d.cache.Get(cacheKey); ok {
		return d.validateDNSResult(endpoint, cached)
	}
	
	var result interface{}
	var err error
	
	switch recordType {
	case "A":
		result, err = d.resolver.LookupIPAddr(ctx, endpoint.Host)
	case "AAAA":
		result, err = d.resolver.LookupIPAddr(ctx, endpoint.Host)
	case "CNAME":
		result, err = d.resolver.LookupCNAME(ctx, endpoint.Host)
	default:
		return fmt.Errorf("unsupported record type: %s", recordType)
	}
	
	if err != nil {
		return fmt.Errorf("dns lookup failed: %w", err)
	}
	
	// Cache the result
	d.cache.Set(cacheKey, result, 5*time.Minute)
	
	return d.validateDNSResult(endpoint, result)
}

func (d *DNSChecker) validateDNSResult(endpoint Endpoint, result interface{}) error {
	recordType := strings.ToUpper(endpoint.RecordType)
	
	switch recordType {
	case "A", "AAAA":
		ips := result.([]net.IPAddr)
		if len(ips) == 0 {
			return errors.New("no IP addresses returned")
		}
		
		if endpoint.Expected != "" {
			expectedIP := net.ParseIP(endpoint.Expected)
			if expectedIP == nil {
				return fmt.Errorf("invalid expected IP: %s", endpoint.Expected)
			}
			
			for _, ip := range ips {
				if ip.IP.Equal(expectedIP) {
					return nil
				}
			}
			return fmt.Errorf("expected IP %s not found", endpoint.Expected)
		}
		
	case "CNAME":
		cname := strings.TrimSuffix(result.(string), ".")
		if endpoint.Expected != "" {
			expected := strings.TrimSuffix(endpoint.Expected, ".")
			if cname != expected {
				return fmt.Errorf("CNAME mismatch: got %s, want %s", cname, expected)
			}
		}
	}
	
	return nil
}

// Type returns the checker type
func (d *DNSChecker) Type() string {
	return "dns"
}

// TCPChecker implements TCP endpoint checking
type TCPChecker struct {
	timeout time.Duration
	dialer  *net.Dialer
}

// NewTCPChecker creates a new TCP checker
func NewTCPChecker(timeout time.Duration) *TCPChecker {
	return &TCPChecker{
		timeout: timeout,
		dialer: &net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		},
	}
}

// Check performs a TCP check
func (t *TCPChecker) Check(ctx context.Context, endpoint Endpoint) error {
	address := endpoint.Address
	if address == "" {
		address = fmt.Sprintf("%s:%d", endpoint.Host, endpoint.Port)
	}
	
	conn, err := t.dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("tcp dial failed: %w", err)
	}
	
	// Set deadline for connection close
	conn.SetDeadline(time.Now().Add(time.Second))
	conn.Close()
	
	return nil
}

// Type returns the checker type
func (t *TCPChecker) Type() string {
	return "tcp"
}

// CheckerFactory creates checkers based on endpoint type
type CheckerFactory struct {
	httpChecker *HTTPChecker
	dnsChecker  *DNSChecker
	tcpChecker  *TCPChecker
}

// NewCheckerFactory creates a new checker factory
func NewCheckerFactory(cfg *MonitorConfig) *CheckerFactory {
	return &CheckerFactory{
		httpChecker: NewHTTPChecker(cfg.RequestTimeout, cfg.MaxResponseBodySize),
		dnsChecker:  NewDNSChecker(cfg.DNSTimeout),
		tcpChecker:  NewTCPChecker(cfg.TCPTimeout),
	}
}

// GetChecker returns a checker for the given endpoint type
func (f *CheckerFactory) GetChecker(endpointType string) (Checker, error) {
	switch strings.ToLower(endpointType) {
	case "http", "https", "":
		return f.httpChecker, nil
	case "dns":
		return f.dnsChecker, nil
	case "tcp":
		return f.tcpChecker, nil
	default:
		return nil, fmt.Errorf("unknown endpoint type: %s", endpointType)
	}
}

// NotifyEvent represents a notification event
type NotifyEvent struct {
	Endpoint      string        `json:"endpoint"`
	IsDown        bool          `json:"is_down"`
	Timestamp     time.Time     `json:"timestamp"`
	FailTime      time.Time     `json:"fail_time,omitempty"`
	ResponseTime  time.Duration `json:"response_time"`
	Error         string        `json:"error,omitempty"`
}

// Notifier interface for sending notifications
type Notifier interface {
	Send(ctx context.Context, events []*NotifyEvent) error
	Name() string
}

// TelegramNotifier sends notifications to Telegram
type TelegramNotifier struct {
	token      string
	chatID     string
	httpClient *http.Client
	limiter    *rate.Limiter
}

// NewTelegramNotifier creates a new Telegram notifier
func NewTelegramNotifier(token, chatID string) *TelegramNotifier {
	return &TelegramNotifier{
		token:  token,
		chatID: chatID,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		limiter: rate.NewLimiter(rate.Every(time.Second), 30), // Telegram rate limit
	}
}

// Send sends notifications to Telegram
func (t *TelegramNotifier) Send(ctx context.Context, events []*NotifyEvent) error {
	if err := t.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limit: %w", err)
	}
	
	message := formatNotificationMessage(events)
	
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", t.token)
	payload := map[string]interface{}{
		"chat_id":    t.chatID,
		"text":       message,
		"parse_mode": "Markdown",
	}
	
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}
	
	return nil
}

// Name returns the notifier name
func (t *TelegramNotifier) Name() string {
	return "Telegram"
}

// formatNotificationMessage formats events into a notification message
func formatNotificationMessage(events []*NotifyEvent) string {
	var sb strings.Builder
	sb.Grow(512) // Pre-allocate
	
	downEvents := make([]*NotifyEvent, 0, len(events))
	upEvents := make([]*NotifyEvent, 0, len(events))
	
	for _, e := range events {
		if e.IsDown {
			downEvents = append(downEvents, e)
		} else {
			upEvents = append(upEvents, e)
		}
	}
	
	if len(downEvents) > 0 {
		sb.WriteString("🔴 *Services DOWN:*\n")
		for _, e := range downEvents {
			fmt.Fprintf(&sb, "• %s\n", e.Endpoint)
			if e.Error != "" {
				fmt.Fprintf(&sb, "  Error: %s\n", e.Error)
			}
			fmt.Fprintf(&sb, "  Failed at: %s\n", e.FailTime.Format("15:04:05"))
		}
	}
	
	if len(upEvents) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString("✅ *Services RECOVERED:*\n")
		for _, e := range upEvents {
			downtime := e.Timestamp.Sub(e.FailTime).Round(time.Second)
			fmt.Fprintf(&sb, "• %s\n", e.Endpoint)
			fmt.Fprintf(&sb, "  Downtime: %s\n", downtime)
		}
	}
	
	return sb.String()
}

// Monitor is the main monitoring service
type Monitor struct {
	config          *Config
	monitorConfig   *MonitorConfig
	checkerFactory  *CheckerFactory
	states          *StateManager
	notifiers       []Notifier
	notifyQueue     chan *NotifyEvent
	semaphore       *semaphore.Weighted
	metrics         *Metrics
	wg              sync.WaitGroup
	cancel          context.CancelFunc
}

// StateManager manages service states safely
type StateManager struct {
	states sync.Map // map[string]*ServiceState
}

// NewStateManager creates a new state manager
func NewStateManager() *StateManager {
	return &StateManager{}
}

// Get returns a service state
func (sm *StateManager) Get(id string) *ServiceState {
	if v, ok := sm.states.Load(id); ok {
		return v.(*ServiceState)
	}
	
	// Create new state if not exists
	state := NewServiceState()
	actual, _ := sm.states.LoadOrStore(id, state)
	return actual.(*ServiceState)
}

// Metrics tracks monitoring metrics
type Metrics struct {
	checksTotal      atomic.Uint64
	checksFailedTotal atomic.Uint64
	checkDuration    atomic.Value // time.Duration
	notificationsSent atomic.Uint64
	goroutines       atomic.Int32
}

// NewMonitor creates a new monitor instance
func NewMonitor(cfg *Config, mcfg *MonitorConfig) (*Monitor, error) {
	checkerFactory := NewCheckerFactory(mcfg)
	
	// Validate endpoints
	for i, ep := range cfg.Endpoints {
		if err := validateEndpoint(ep); err != nil {
			return nil, fmt.Errorf("endpoint %d: %w", i, err)
		}
	}
	
	ctx, cancel := context.WithCancel(context.Background())
	
	m := &Monitor{
		config:         cfg,
		monitorConfig:  mcfg,
		checkerFactory: checkerFactory,
		states:         NewStateManager(),
		notifiers:      make([]Notifier, 0),
		notifyQueue:    make(chan *NotifyEvent, 1000), // Buffered queue
		semaphore:      semaphore.NewWeighted(int64(mcfg.MaxConcurrentChecks)),
		metrics:        &Metrics{},
		cancel:         cancel,
	}
	
	m.metrics.checkDuration.Store(time.Duration(0))
	
	return m, nil
}

// AddNotifier adds a notifier to the monitor
func (m *Monitor) AddNotifier(n Notifier) {
	m.notifiers = append(m.notifiers, n)
}

// Start starts the monitoring service
func (m *Monitor) Start(ctx context.Context) error {
	slog.Info("starting monitor service",
		"version", Version,
		"endpoints", len(m.config.Endpoints),
		"interval", m.monitorConfig.CheckInterval)
	
	// Start notification worker
	m.wg.Add(1)
	go m.notificationWorker(ctx)
	
	// Start metrics reporter
	if m.monitorConfig.EnableMetrics {
		m.wg.Add(1)
		go m.metricsReporter(ctx)
	}
	
	// Start monitoring loop
	ticker := time.NewTicker(m.monitorConfig.CheckInterval)
	defer ticker.Stop()
	
	// Initial check
	m.checkAllEndpoints(ctx)
	
	for {
		select {
		case <-ticker.C:
			m.checkAllEndpoints(ctx)
		case <-ctx.Done():
			return m.shutdown()
		}
	}
}

// checkAllEndpoints checks all configured endpoints concurrently
func (m *Monitor) checkAllEndpoints(ctx context.Context) {
	start := time.Now()
	
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(m.monitorConfig.MaxConcurrentChecks)
	
	for _, endpoint := range m.config.Endpoints {
		endpoint := endpoint // Capture loop variable
		
		g.Go(func() error {
			// Acquire semaphore
			if err := m.semaphore.Acquire(ctx, 1); err != nil {
				return err
			}
			defer m.semaphore.Release(1)
			
			m.checkEndpoint(ctx, endpoint)
			return nil
		})
	}
	
	// Wait for all checks to complete
	if err := g.Wait(); err != nil && err != context.Canceled {
		slog.Error("check cycle error", "error", err)
	}
	
	duration := time.Since(start)
	m.metrics.checkDuration.Store(duration)
	
	slog.Debug("check cycle completed",
		"duration", duration,
		"endpoints", len(m.config.Endpoints))
}

// checkEndpoint checks a single endpoint
func (m *Monitor) checkEndpoint(ctx context.Context, endpoint Endpoint) {
	m.metrics.checksTotal.Add(1)
	
	checker, err := m.checkerFactory.GetChecker(endpoint.Type)
	if err != nil {
		slog.Error("get checker failed", "error", err)
		return
	}
	
	// Create timeout context for this check
	checkCtx, cancel := context.WithTimeout(ctx, m.monitorConfig.RequestTimeout)
	defer cancel()
	
	// Perform check with retry
	start := time.Now()
	err = checker.Check(checkCtx, endpoint)
	responseTime := time.Since(start)
	
	// Retry logic with exponential backoff
	if err != nil && checkCtx.Err() == nil {
		time.Sleep(m.monitorConfig.RetryDelay)
		
		retryCtx, retryCancel := context.WithTimeout(ctx, m.monitorConfig.RequestTimeout)
		defer retryCancel()
		
		start = time.Now()
		err = checker.Check(retryCtx, endpoint)
		responseTime = time.Since(start)
	}
	
	// Update state
	success := err == nil
	if !success {
		m.metrics.checksFailedTotal.Add(1)
	}
	
	state := m.states.Get(endpoint.GetIdentifier())
	stateChanged, isDown := state.Update(success, responseTime, err)
	
	if stateChanged {
		// Send notification
		event := &NotifyEvent{
			Endpoint:     endpoint.GetIdentifier(),
			IsDown:       isDown,
			Timestamp:    time.Now(),
			ResponseTime: responseTime,
		}
		
		if isDown {
			event.FailTime = state.firstFailTime.Load().(time.Time)
			if err != nil {
				event.Error = err.Error()
			}
		} else {
			event.FailTime = state.firstFailTime.Load().(time.Time)
		}
		
		select {
		case m.notifyQueue <- event:
		case <-ctx.Done():
			return
		default:
			slog.Warn("notification queue full, dropping event",
				"endpoint", endpoint.GetIdentifier())
		}
	}
	
	// Log result
	if success {
		slog.Debug("check successful",
			"endpoint", endpoint.GetIdentifier(),
			"response_time", responseTime)
	} else {
		slog.Warn("check failed",
			"endpoint", endpoint.GetIdentifier(),
			"error", err,
			"response_time", responseTime)
	}
}

// notificationWorker processes notification events
func (m *Monitor) notificationWorker(ctx context.Context) {
	defer m.wg.Done()
	
	batch := make([]*NotifyEvent, 0, m.monitorConfig.MaxBatchSize)
	timer := time.NewTimer(m.monitorConfig.NotifyBatchWindow)
	timer.Stop()
	
	send := func() {
		if len(batch) == 0 {
			return
		}
		
		for _, notifier := range m.notifiers {
			notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := notifier.Send(notifyCtx, batch)
			cancel()
			
			if err != nil {
				slog.Error("notification failed",
					"notifier", notifier.Name(),
					"error", err)
				continue
			}
			
			m.metrics.notificationsSent.Add(uint64(len(batch)))
			slog.Info("notifications sent",
				"notifier", notifier.Name(),
				"count", len(batch))
			break
		}
		
		batch = batch[:0]
	}
	
	for {
		select {
		case event := <-m.notifyQueue:
			batch = append(batch, event)
			
			if len(batch) >= m.monitorConfig.MaxBatchSize {
				timer.Stop()
				send()
			} else if len(batch) == 1 {
				timer.Reset(m.monitorConfig.NotifyBatchWindow)
			}
			
		case <-timer.C:
			send()
			
		case <-ctx.Done():
			timer.Stop()
			send() // Send any pending notifications
			return
		}
	}
}

// metricsReporter reports metrics periodically
func (m *Monitor) metricsReporter(ctx context.Context) {
	defer m.wg.Done()
	
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			var memStats runtime.MemStats
			runtime.ReadMemStats(&memStats)
			
			slog.Info("metrics",
				"checks_total", m.metrics.checksTotal.Load(),
				"checks_failed", m.metrics.checksFailedTotal.Load(),
				"check_duration", m.metrics.checkDuration.Load().(time.Duration),
				"notifications_sent", m.metrics.notificationsSent.Load(),
				"goroutines", runtime.NumGoroutine(),
				"memory_alloc_mb", memStats.Alloc/1024/1024,
				"memory_sys_mb", memStats.Sys/1024/1024,
				"gc_runs", memStats.NumGC)
			
			// Force GC if memory usage is high
			if memStats.Alloc > 500*1024*1024 {
				slog.Warn("high memory usage, forcing GC",
					"alloc_mb", memStats.Alloc/1024/1024)
				runtime.GC()
				debug.FreeOSMemory()
			}
			
		case <-ctx.Done():
			return
		}
	}
}

// shutdown performs graceful shutdown
func (m *Monitor) shutdown() error {
	slog.Info("shutting down monitor service")
	
	// Cancel context
	m.cancel()
	
	// Close notification queue
	close(m.notifyQueue)
	
	// Wait for workers with timeout
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	
	select {
	case <-done:
		slog.Info("shutdown completed")
		return nil
	case <-time.After(m.monitorConfig.ShutdownTimeout):
		return errors.New("shutdown timeout exceeded")
	}
}

// Helper functions

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	if val := os.Getenv(key); val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
		slog.Warn("invalid duration",
			"key", key,
			"value", val,
			"using", defaultVal)
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		var i int
		if _, err := fmt.Sscanf(val, "%d", &i); err == nil && i > 0 {
			return i
		}
		slog.Warn("invalid integer",
			"key", key,
			"value", val,
			"using", defaultVal)
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	if val := os.Getenv(key); val != "" {
		switch strings.ToLower(val) {
		case "true", "1", "yes", "on":
			return true
		case "false", "0", "no", "off":
			return false
		default:
			slog.Warn("invalid boolean",
				"key", key,
				"value", val,
				"using", defaultVal)
		}
	}
	return defaultVal
}

func validateURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	
	// Check scheme
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("scheme must be http or https")
	}
	
	// Check host
	if parsed.Host == "" {
		return errors.New("host is required")
	}
	
	// Prevent SSRF attacks
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		host = parsed.Host
	}
	
	ip := net.ParseIP(host)
	if ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return errors.New("private/local IP addresses are not allowed")
		}
	}
	
	return nil
}

func sanitizeURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "[invalid-url]"
	}
	parsed.RawQuery = ""
	parsed.User = nil
	parsed.Fragment = ""
	return parsed.String()
}

func validateEndpoint(ep Endpoint) error {
	if ep.Type == "" {
		ep.Type = "http"
	}
	
	switch strings.ToLower(ep.Type) {
	case "http", "https":
		if ep.URL == "" {
			return errors.New("URL is required")
		}
		return validateURL(ep.URL)
		
	case "dns":
		if ep.Host == "" {
			return errors.New("host is required")
		}
		if ep.RecordType == "" {
			return errors.New("record_type is required")
		}
		validTypes := map[string]bool{"A": true, "AAAA": true, "CNAME": true}
		if !validTypes[strings.ToUpper(ep.RecordType)] {
			return fmt.Errorf("unsupported record type: %s", ep.RecordType)
		}
		
	case "tcp":
		if ep.Address == "" && ep.Host == "" {
			return errors.New("address or host is required")
		}
		if ep.Address == "" {
			if ep.Port <= 0 || ep.Port > 65535 {
				return fmt.Errorf("invalid port: %d", ep.Port)
			}
		}
		
	default:
		return fmt.Errorf("unknown endpoint type: %s", ep.Type)
	}
	
	return nil
}

// LoadConfig loads configuration from file
func LoadConfig(path string) (*Config, error) {
	// Limit config file size to prevent DoS
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat config file: %w", err)
	}
	
	if info.Size() > 10*1024*1024 { // 10MB limit
		return nil, errors.New("config file too large")
	}
	
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("no endpoints configured")
	}
	
	if len(cfg.Endpoints) > 1000 {
		return nil, errors.New("too many endpoints (max 1000)")
	}
	
	// Normalize endpoint types
	for i := range cfg.Endpoints {
		if cfg.Endpoints[i].Type == "" {
			cfg.Endpoints[i].Type = "http"
		}
		cfg.Endpoints[i].Type = strings.ToLower(cfg.Endpoints[i].Type)
	}
	
	return &cfg, nil
}

func main() {
	// Setup structured logging
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
		AddSource: false,
	}
	
	if os.Getenv("DEBUG") == "true" {
		opts.Level = slog.LevelDebug
	}
	
	handler := slog.NewJSONHandler(os.Stdout, opts)
	slog.SetDefault(slog.New(handler))
	
	// Log startup
	slog.Info("starting monitor service",
		"version", Version,
		"go_version", runtime.Version(),
		"pid", os.Getpid())
	
	// Run service
	if err := run(); err != nil {
		slog.Error("service failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Load monitor configuration
	monitorConfig, err := LoadMonitorConfig()
	if err != nil {
		return fmt.Errorf("load monitor config: %w", err)
	}
	
	// Load service configuration
	configPath := os.Getenv("CONFIG_PATH")
	if configPath == "" {
		configPath = "config.yaml"
	}
	
	config, err := LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	
	// Validate Telegram configuration
	telegramToken := os.Getenv("TELEGRAM_BOT_TOKEN")
	telegramChatID := os.Getenv("TELEGRAM_CHAT_ID")
	
	if telegramToken == "" || telegramChatID == "" {
		return errors.New("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID are required")
	}
	
	// Create monitor
	monitor, err := NewMonitor(config, monitorConfig)
	if err != nil {
		return fmt.Errorf("create monitor: %w", err)
	}
	
	// Add notifiers
	monitor.AddNotifier(NewTelegramNotifier(telegramToken, telegramChatID))
	
	// Add Mattermost if configured
	if mattermostURL := os.Getenv("MATTERMOST_WEBHOOK_URL"); mattermostURL != "" {
		// TODO: Implement MattermostNotifier
		slog.Info("mattermost webhook configured")
	}
	
	// Setup signal handling
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	
	// Start monitor
	return monitor.Start(ctx)
}
