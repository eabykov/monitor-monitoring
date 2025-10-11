# Monitor Monitoring

[![Go Report Card](https://goreportcard.com/badge/github.com/eabykov/monitor-monitoring)](https://goreportcard.com/report/github.com/eabykov/monitor-monitoring)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/eabykov/monitor-monitoring)
![License](https://img.shields.io/github/license/eabykov/monitor-monitoring)

A lightweight, concurrent HTTP(S) service monitor written in Go.  

It continuously checks configured endpoints, batches state changes, and sends **Telegram** alerts (with optional **Mattermost** fallback) whenever a service goes **DOWN** or **RECOVERED**.

## ✨ Features

| Feature | Description |
||-|
| ⚡ **Concurrent checks** | All endpoints are checked in parallel |
| 🔄 **Automatic retry** | Failed checks are retried once after a configurable delay |
| 📊 **Batching & de-duplication** | Notifications are batched per window to avoid spam |
| 📱 **Telegram** | Native Markdown alerts via Telegram Bot API |
| 🦝 **Mattermost fallback** | Optional webhook fallback if Telegram fails |
| 🛠️ **12-factor config** | Environment variables for runtime tuning |
| 🧘 **Graceful shutdown** | Handles `SIGINT`/`SIGTERM` cleanly |
| 🪵 **Structured logging** | JSON-ish logs via `slog` |

## 🚀 Quick Start

### 1. Get the binary

```bash
# Clone
git clone https://github.com/eabykov/monitor-monitoring.git
cd monitor-monitoring

# Build
go build -o monitor-monitoring .
```

### 2. Create a Telegram bot

1. Message [@BotFather](https://t.me/botfather) → `/newbot`  
2. Save the token  
3. Add the bot to your channel/group  
4. Get chat ID:  
   `https://api.telegram.org/bot<TOKEN>/getUpdates`  
   (or use [@userinfobot](https://t.me/userinfobot))

### 3. Minimal `config.yaml`

```yaml
endpoints:
  - url: https://api.github.com
    method: GET
    expected_status: 200
```

### 4. Run

```bash
export TELEGRAM_BOT_TOKEN="123456:ABC-DEF..."
export TELEGRAM_CHAT_ID="-1001234567890"
./monitor-monitoring
```



## ⚙️ Configuration

### Environment variables

| Variable | Default | Purpose |
|-|||
| `CHECK_INTERVAL` | `1m` | How often to probe all endpoints |
| `REQUEST_TIMEOUT` | `45s` | HTTP client timeout per check |
| `RETRY_DELAY` | `5s` | Wait before retrying a failed check |
| `FAILURE_THRESHOLD` | `3` | Consecutive failures to mark DOWN |
| `NOTIFY_BATCH_WINDOW` | `10s` | Max time to wait before sending a batch |
| `CONFIG_PATH` | `config.yaml` | Path to YAML endpoint list |
| `MATTERMOST_WEBHOOK_URL` | *empty* | Webhook URL for fallback notifications |

### YAML schema (`config.yaml`)

```yaml
endpoints:
  - url: https://example.com/health
    method: GET              # optional, defaults to GET
    expected_status: 200     # optional, defaults to 200
    headers:                 # optional
      Authorization: Bearer secret
```

## 📦 Deployment

### Docker

Build & run:

```bash
docker build -t monitor-monitoring .
docker run -d \
  -e TELEGRAM_BOT_TOKEN \
  -e TELEGRAM_CHAT_ID \
  -e MATTERMOST_WEBHOOK_URL \
  -v $(pwd)/config.yaml:/etc/monitor/config.yaml \
  --name monitor-monitoring \
  monitor-monitoring
```

### systemd (Linux)

```ini
# /etc/systemd/system/monitor-monitoring.service
[Unit]
Description=HTTP Service Monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=monitor
Group=monitor
WorkingDirectory=/opt/monitor
ExecStart=/opt/monitor/monitor-monitoring
Environment="TELEGRAM_BOT_TOKEN=___"
Environment="TELEGRAM_CHAT_ID=___"
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

## 🧪 Testing

```bash
go test ./...
```

## 🔍 Troubleshooting

| Symptom | Fix |
||--|
| `TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID must be set` | Export both variables |
| Telegram messages not arriving | Check bot is in channel and chat ID is correct |
| `context deadline exceeded` | Increase `REQUEST_TIMEOUT` |
| High memory usage | Lower `MaxIdleConnsPerHost` in code or switch to `fasthttp` fork |

## 🤝 Contributing

1. Fork the repo  
2. Create a feature branch (`git checkout -b feat/amazing`)  
3. Commit with [conventional messages](https://www.conventionalcommits.org)  
4. Push and open a Pull Request  
5. Ensure CI passes (`golangci-lint run`, `go test`, `go mod tidy`)

## 📄 License

MIT © [eabykov](https://github.com/eabykov)

## 🔗 Links

| Resource | URL |
|-|--|
| 🐛 Issues | https://github.com/eabykov/monitor-monitoring/issues |
| 💬 Discussions | https://github.com/eabykov/monitor-monitoring/discussions |
```
