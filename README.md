# Juicebox Alerts

Send alerts to one or more Discord channel whenever your chosen Juicebox project(s) receives a payment, or new Juicebox projects are created.

I have an instance of this running. If you'd like me to add your project/server to it, message me on Discord. My handle is `filipvv`.

## Usage

1. Copy `.example.env` to `.env` and fill out the variables.
2. Create your `config.json` like so:

```json
{
    "2814798721487298472": ["2", "5", "100"],
    "1982738927137337731": ["*"],
    "4987187598147398641": ["new"]
}
```

In the example above:
- Discord channel ID `2814798721487298472` will receive notifications when Juicebox projects with IDs 2, 5, or 100 are paid.
- Channel ID `1982738927137337731` will receive notifications when any project is paid.
- Channel ID `4987187598147398641` will receive notifications when new Juicebox projects are created.

3. Build with `go build .` or run with `go run .`.