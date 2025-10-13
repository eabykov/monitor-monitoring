# Monitor Monitoring [![Go Report Card](https://goreportcard.com/badge/github.com/eabykov/monitor-monitoring)](https://goreportcard.com/report/github.com/eabykov/monitor-monitoring) ![GitHub release (latest SemVer)](https://img.shields.io/github/v/release/eabykov/monitor-monitoring?sort=semver) ![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/eabykov/monitor-monitoring) ![License](https://img.shields.io/github/license/eabykov/monitor-monitoring)

Less than 7MB binary that outperforms enterprise monitoring giants consuming gigabytes of resources. While competitors require entire SRE/DevOps teams, this Go masterpiece delivers instant Telegram alerts for HTTP, DNS, and TCP in milliseconds with zero dependencies.

## ✨ Features

- ⚡ **Blazing concurrent checks** - All endpoints monitored in parallel with configurable concurrency limits
- 🌐 **Multi-protocol mastery** - HTTP/HTTPS, DNS (A/AAAA/CNAME), and TCP monitoring in one unified tool
- 🎯 **Smart failure detection** - Configurable failure thresholds with automatic retry logic
- 📱 **Instant Telegram alerts** - Rich Markdown notifications with downtime tracking and recovery reports
- 🦝 **Mattermost fallback** - Redundant notification channels ensure you never miss critical alerts
- 📊 **Intelligent batching** - Anti-spam protection with configurable batch windows and size limits
- 🛡️ **Production-hardened** - Memory monitoring, graceful shutdown, structured logging, and resource optimization
- 🔧 **Zero-config deployment** - Environment variable configuration following 12-factor app principles
- 🔄 **Smart retry mechanism** - Failed checks get a second chance with configurable delays
- 📈 **Built-in observability** - Memory usage tracking, performance metrics, and detailed debug logging

## 🚀 Quick Start

### 1. Create Telegram Bot

**Create your monitoring bot:**
1. Open Telegram and message [@BotFather](https://t.me/BotFather)
2. Send `/newbot` command
3. Choose a name and username for your bot
4. **Save the bot token** (format: `123456789:ABC-DEF1234ghIkl-zyx57W2v1u123ew11`)

**Get your chat ID:**
- **Option A**: Add [@userinfobot](https://t.me/userinfobot) to your group/channel, then send any message
- **Option B**: Send a test message to your bot, then visit:
  ```
  https://api.telegram.org/bot<YOUR_BOT_TOKEN>/getUpdates
  ```
  Look for `"chat":{"id":-1001234567890}` in the response

**Add bot to your channel/group:**
- Add your bot as an administrator to receive notifications

### 2. Install Monitor Monitoring

**Download and build:**
```bash
git clone https://github.com/eabykov/monitor-monitoring.git
cd monitor-monitoring
go build -o monitor-monitoring .
```

### 3. Configure Your Endpoints

Create `config.yaml`:
```yaml
endpoints:
  # Quick HTTP check
  - url: https://api.github.com
    type: http
    method: GET
    expected_status: 200
```

### 4. Set Environment Variables

```bash
export TELEGRAM_BOT_TOKEN="123456789:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"
export TELEGRAM_CHAT_ID="-1001234567890"
```

### 5. Launch Your Monitor

```bash
./monitor-monitoring
```

**Expected output:**
```
2024/01/15 14:30:25 INFO loaded configuration endpoints=1
2024/01/15 14:30:25 INFO monitoring endpoint type=HTTP url=https://api.github.com method=GET expected_status=200 has_custom_headers=false
2024/01/15 14:30:25 INFO starting service monitor hostname=web01 interval=1m0s gomaxprocs=8
```

## ⚙️ Configuration

### Environment Variables

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| `TELEGRAM_BOT_TOKEN` | - | **Yes** | Your Telegram bot token from @BotFather |
| `TELEGRAM_CHAT_ID` | - | **Yes** | Chat ID where notifications will be sent |
| `CHECK_INTERVAL` | `60s` | No | How often to check all endpoints |
| `REQUEST_TIMEOUT` | `20s` | No | HTTP request timeout per check |
| `RETRY_DELAY` | `10s` | No | Wait time before retrying failed checks |
| `FAILURE_THRESHOLD` | `2` | No | Consecutive failures before marking service DOWN |
| `NOTIFY_BATCH_WINDOW` | `45s` | No | Maximum time to wait before sending notification batch |
| `MAX_BATCH_SIZE` | `200` | No | Maximum number of notifications in one batch |
| `MAX_CONCURRENT_CHECKS` | `32` | No | Maximum parallel health checks |
| `MAX_RESPONSE_BODY_SIZE` | `2097152` | No | Maximum HTTP response body size in bytes (2MB) |
| `DNS_TIMEOUT` | `5s` | No | Timeout for DNS queries |
| `TCP_TIMEOUT` | `10s` | No | Timeout for TCP connection attempts |
| `CONFIG_PATH` | `config.yaml` | No | Path to YAML configuration file |
| `MATTERMOST_WEBHOOK_URL` | - | No | Mattermost webhook URL for fallback notifications |
| `MONITOR_HOSTNAME` | (auto) | No | Rewrite the default hostname that will be displayed in notifications |

### YAML Schema (`config.yaml`)

#### HTTP/HTTPS Endpoints
Monitor web services, APIs, and websites:

```yaml
endpoints:
  # Basic HTTP check
  - url: https://example.com
    type: http                    # Optional: defaults to 'http'
    method: GET                   # Optional: GET, POST, PUT, DELETE, HEAD, PATCH, OPTIONS
    expected_status: 200          # Optional: defaults to 200
    
  # API with authentication
  - url: https://api.example.com/health
    type: http
    method: GET
    expected_status: 200
    headers:                      # Optional: custom headers
      Authorization: Bearer your-token-here
      Accept: application/json
      User-Agent: monitor-monitoring/1.0
      
  # POST endpoint check
  - url: https://api.example.com/webhook
    type: http
    method: POST
    expected_status: 201
    headers:
      Content-Type: application/json
```

#### DNS Monitoring
Verify DNS resolution and validate records:

```yaml
endpoints:
  # Basic A record check
  - type: dns
    host: example.com             # Required: hostname to resolve
    record_type: A                # Required: A, AAAA, or CNAME
    
  # A record with validation
  - type: dns
    host: api.example.com
    record_type: A
    expected: 192.168.1.100       # Optional: validate specific IP
    
  # IPv6 AAAA record
  - type: dns
    host: ipv6.example.com
    record_type: AAAA
    expected: 2001:db8::1         # Optional: validate specific IPv6
    
  # CNAME record check
  - type: dns
    host: www.example.com
    record_type: CNAME
    expected: example.com         # Optional: validate CNAME target
    
  # Multiple DNS servers check
  - type: dns
    host: google.com
    record_type: A                # Will resolve to multiple IPs
    
  # Cloudflare DNS check
  - type: dns
    host: one.one.one.one
    record_type: A
    expected: 1.1.1.1
```

#### TCP Port Monitoring
Check network service availability:

```yaml
endpoints:
  # Database connection
  - type: tcp
    host: db.example.com          # Required: hostname
    port: 5432                    # Required: port number
    
  # Alternative address format
  - type: tcp
    address: "database.company.com:5432"  # Alternative: full address
    
  # Web server
  - type: tcp
    host: web.example.com
    port: 443
    
  # SMTP server
  - type: tcp
    host: mail.example.com
    port: 587
    
  # Redis cache
  - type: tcp
    address: "redis.example.com:6379"
    
  # Custom application
  - type: tcp
    host: app.example.com
    port: 8080
```

#### Mixed Configuration Example
Real-world monitoring setup:

```yaml
endpoints:
  # Frontend website
  - url: https://www.company.com
    type: http
    method: GET
    expected_status: 200
    
  # API health endpoint
  - url: https://api.company.com/health
    type: http
    expected_status: 200
    headers:
      Authorization: Bearer monitoring-token
      
  # Database connectivity
  - type: tcp
    host: prod-db.company.com
    port: 5432
    
  # DNS resolution
  - type: dns
    host: company.com
    record_type: A
    expected: 203.0.113.10
    
  # CDN endpoint
  - url: https://cdn.company.com/status
    type: http
    expected_status: 200
    
  # Email server
  - type: tcp
    host: smtp.company.com
    port: 587
```
