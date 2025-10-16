# Monitor Monitoring

![Stars](https://img.shields.io/github/stars/eabykov/monitor-monitoring?style=flat)
[![Go Report Card](https://goreportcard.com/badge/github.com/eabykov/monitor-monitoring)](https://goreportcard.com/report/github.com/eabykov/monitor-monitoring)
![GitHub release (latest SemVer)](https://img.shields.io/github/v/release/eabykov/monitor-monitoring?sort=semver)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/eabykov/monitor-monitoring)
![License](https://img.shields.io/github/license/eabykov/monitor-monitoring)

Lightweight monitoring solution in a single 7MB binary. Monitors HTTP/HTTPS endpoints, DNS records, and TCP ports with instant notifications via Telegram, Slack, Discord, or Mattermost.

## Features

- **Concurrent monitoring** - Parallel endpoint checks with configurable concurrency limits
- **Multi-protocol support** - HTTP/HTTPS, DNS (A/AAAA/CNAME), and TCP monitoring
- **Smart failure detection** - Configurable failure thresholds with automatic retry logic
- **Multi-channel notifications** - Telegram, Slack, Discord, and Mattermost support with fallback mechanism
- **Intelligent batching** - Anti-spam protection with configurable batch windows and size limits
- **Production-ready** - Memory monitoring, graceful shutdown, structured logging
- **Zero dependencies** - Single binary with no external runtime dependencies
- **12-factor app** - Environment variable configuration
- **Built-in retry** - Automatic retry on failed checks with configurable delays

## Quick Start

### 1. Setup Notification Channel

#### Telegram (Recommended)

**Create bot:**
1. Message [@BotFather](https://t.me/BotFather) on Telegram
2. Send `/newbot` and follow prompts
3. Save the bot token (format: `123456789:ABC-DEF1234ghIkl-zyx57W2v1u123ew11`)

**Get chat ID:**
- Add [@userinfobot](https://t.me/userinfobot) to your group/channel
- Send any message and copy the ID shown
- Or visit: `https://api.telegram.org/bot<YOUR_TOKEN>/getUpdates` after sending a message to your bot

**Add bot to channel/group:**
- Add your bot as administrator to receive notifications

#### Slack

1. Create incoming webhook at `https://api.slack.com/messaging/webhooks`
2. Copy webhook URL (format: `https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXX`)

#### Discord

1. Edit channel → Integrations → Webhooks → New Webhook
2. Copy webhook URL (format: `https://discord.com/api/webhooks/123456789012345678/abcdefghijklmnopqrstuvwxyz`)

#### Mattermost

1. Integrations → Incoming Webhooks → Add Incoming Webhook
2. Copy webhook URL (format: `https://mattermost.example.com/hooks/xxx`)

### 2. Install

```bash
git clone https://github.com/eabykov/monitor-monitoring.git
cd monitor-monitoring
go build -o monitor-monitoring .
```

### 3. Configure Endpoints

Create `config.yaml`:

```yaml
endpoints:
  - url: https://api.github.com
    type: http
    method: GET
    expected_status: 200
```

### 4. Set Environment Variables

**Telegram:**
```bash
export TELEGRAM_BOT_TOKEN="123456789:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"
export TELEGRAM_CHAT_ID="-1001234567890"
```

**Slack:**
```bash
export PRIMARY_NOTIFIER="slack"
export SLACK_WEBHOOK_URL="https://hooks.slack.com/services/YOUR/WEBHOOK/URL"
```

**Discord:**
```bash
export PRIMARY_NOTIFIER="discord"
export DISCORD_WEBHOOK_URL="https://discord.com/api/webhooks/YOUR/WEBHOOK/URL"
```

**Mattermost:**
```bash
export PRIMARY_NOTIFIER="mattermost"
export MATTERMOST_WEBHOOK_URL="https://mattermost.example.com/hooks/YOUR_WEBHOOK_ID"
```

**With Fallback:**
```bash
export PRIMARY_NOTIFIER="telegram"
export FALLBACK_NOTIFIER="slack"
export TELEGRAM_BOT_TOKEN="your_telegram_token"
export TELEGRAM_CHAT_ID="your_chat_id"
export SLACK_WEBHOOK_URL="your_slack_webhook"
```

### 5. Run

```bash
./monitor-monitoring
```

## Configuration

### Environment Variables

| Variable | Default | Required | Description |
|----------|---------|----------|-------------|
| `PRIMARY_NOTIFIER` | `telegram` | No | Primary notification channel: `telegram`, `slack`, `discord`, `mattermost` |
| `FALLBACK_NOTIFIER` | _(empty)_ | No | Fallback notification channel if primary fails |
| `TELEGRAM_BOT_TOKEN` | - | If using Telegram | Bot token from @BotFather |
| `TELEGRAM_CHAT_ID` | - | If using Telegram | Chat/channel/group ID for notifications |
| `SLACK_WEBHOOK_URL` | - | If using Slack | Incoming webhook URL |
| `DISCORD_WEBHOOK_URL` | - | If using Discord | Webhook URL |
| `MATTERMOST_WEBHOOK_URL` | - | If using Mattermost | Incoming webhook URL |
| `CHECK_INTERVAL` | `60s` | No | Interval between check cycles |
| `REQUEST_TIMEOUT` | `10s` | No | HTTP request timeout |
| `RETRY_DELAY` | `3s` | No | Delay before retrying failed check |
| `FAILURE_THRESHOLD` | `2` | No | Consecutive failures before alerting |
| `NOTIFY_BATCH_WINDOW` | `40s` | No | Maximum wait time before sending batch |
| `MAX_BATCH_SIZE` | `50` | No | Maximum events per notification batch |
| `MAX_CONCURRENT_CHECKS` | `20` | No | Maximum parallel checks |
| `MAX_RESPONSE_BODY_SIZE` | `524288` | No | Max HTTP response body size (bytes) |
| `DNS_TIMEOUT` | `5s` | No | DNS query timeout |
| `TCP_TIMEOUT` | `8s` | No | TCP connection timeout |
| `CONFIG_PATH` | `config.yaml` | No | Path to YAML configuration |
| `MONITOR_HOSTNAME` | _(auto-detected)_ | No | Hostname shown in notifications |

### YAML Configuration (`config.yaml`)

#### HTTP/HTTPS Monitoring

```yaml
endpoints:
  # Basic HTTP check
  - url: https://example.com
    type: http                    # Optional (default: http)
    method: GET                   # Optional (default: GET)
    expected_status: 200          # Optional (default: 200)
    
  # API with authentication
  - url: https://api.example.com/health
    type: http
    method: GET
    expected_status: 200
    headers:
      Authorization: Bearer your-token
      Accept: application/json
      
  # POST endpoint
  - url: https://api.example.com/webhook
    type: http
    method: POST
    expected_status: 201
```

**Supported HTTP methods:** GET, POST, PUT, DELETE, HEAD, PATCH, OPTIONS

#### DNS Monitoring

```yaml
endpoints:
  # A record check
  - type: dns
    host: example.com
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
    expected: 2001:db8::1
    
  # CNAME record
  - type: dns
    host: www.example.com
    record_type: CNAME
    expected: example.com
```

**Supported record types:** A (IPv4), AAAA (IPv6), CNAME

#### TCP Port Monitoring

```yaml
endpoints:
  # Database connection
  - type: tcp
    host: db.example.com
    port: 5432
    
  # Alternative format
  - type: tcp
    address: "redis.example.com:6379"
    
  # HTTPS port
  - type: tcp
    host: web.example.com
    port: 443
```

#### Complete Example

```yaml
endpoints:
  # Website
  - url: https://www.company.com
    type: http
    
  # API health
  - url: https://api.company.com/health
    type: http
    headers:
      Authorization: Bearer monitoring-token
      
  # Database
  - type: tcp
    host: prod-db.company.com
    port: 5432
    
  # DNS
  - type: dns
    host: company.com
    record_type: A
    expected: 203.0.113.10
    
  # Email server
  - type: tcp
    host: smtp.company.com
    port: 587
```

## Docker Deployment

**Dockerfile:**
```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o monitor-monitoring .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/monitor-monitoring .
COPY config.yaml .
CMD ["./monitor-monitoring"]
```

**docker-compose.yml:**
```yaml
services:
  monitor:
    build: .
    environment:
      - TELEGRAM_BOT_TOKEN=${TELEGRAM_BOT_TOKEN}
      - TELEGRAM_CHAT_ID=${TELEGRAM_CHAT_ID}
    volumes:
      - ./config.yaml:/root/config.yaml
    restart: unless-stopped
```

## Systemd Service

**Create `/etc/systemd/system/monitor-monitoring.service`:**

```ini
[Unit]
Description=Monitor Monitoring Service
After=network.target

[Service]
Type=simple
User=monitor
WorkingDirectory=/opt/monitor-monitoring
ExecStart=/opt/monitor-monitoring/monitor-monitoring
Restart=always
RestartSec=10
Environment="TELEGRAM_BOT_TOKEN=your_token"
Environment="TELEGRAM_CHAT_ID=your_chat_id"
MemoryMax=256M
CPUQuota=10%

[Install]
WantedBy=multi-user.target
```

**Enable and start:**
```bash
sudo systemctl daemon-reload
sudo systemctl enable monitor-monitoring
sudo systemctl start monitor-monitoring
```

## Troubleshooting

**No notifications received:**
- Verify bot token and chat ID are correct
- Ensure bot is added to the channel/group as administrator
- Check logs for notification errors
- Test webhook URLs manually with curl

**High memory usage:**
- Reduce `MAX_CONCURRENT_CHECKS`
- Decrease `MAX_RESPONSE_BODY_SIZE`
- Lower `MAX_BATCH_SIZE`

**Frequent false positives:**
- Increase `REQUEST_TIMEOUT`
- Increase `FAILURE_THRESHOLD`
- Check network connectivity

**Missing alerts:**
- Verify `NOTIFY_BATCH_WINDOW` isn't too long
- Check notification queue isn't full
- Review logs for dropped events
