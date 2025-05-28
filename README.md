# Juicebox Alerts

Send alerts to Discord channels when new Juicebox projects are made, or when projects receive payments.

I have an instance running already. If you'd like me to add your project to it:

1. Click [this link](https://discord.com/oauth2/authorize?client_id=1377330627803877436) to install the bot in your server. Make sure it has any roles/permissions needed to view and send messages in the channel you'd like to receive notifications in.
2. On Discord, go to your Settings > Advanced > Enable "Developer Mode".
3. Go to the channel you'd like to receive notifications in. Right click > "Copy Channel ID".
4. Message me on Discord (`filipvv`) with your channel ID and a list of the projects you'd like to receive notifications for.

## Setup

If you'd like to run your own instance:

1. Copy `.example.env` to `.env` and fill out
your Discord bot token (`DISCORD_TOKEN`), a GraphQL endpoint for v1-v3 events (`SUBGRAPH_URL`), and an endpoint for v4 events (`BENDYSTRAW_URL`).
2. Edit `config.json`, which maps Discord channel IDs to notification rules
3. Build with `go build .`, run with `go run .`, or test with `TESTING=1 go run .`

## Configuration

Map Discord channel IDs to notification rules in `config.json`:

```json
{
    "channel_id_1": ["pay", "25", "1:83"],
    "channel_id_2": ["new"],
    "channel_id_3": ["10:456"]
}
```

| Rule | Description |
|------|-------------|
| `"new"` | Notifications when new projects are made |
| `"pay"` | Notifications when any project is paid |
| `"25"` | Notifications when project 25 on v2/v3 is paid |
| `"1:83"` | Notifications when mainnet project 83 on v4 is paid |
| `"10:456"` | Notification when Optimism project 456 on v4 is paid |

Supported networks: Ethereum (1), Optimism (10), Base (8453), Arbitrum (42161)
