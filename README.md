# SF Muni Agent

A CLI chatbot powered by Claude that gives you real-time SF Muni arrival predictions. Ask it about any stop and it'll tell you what's coming and when.

```
$ go run .
Chat with Claude (use 'ctrl-c' to quit)
You: what's coming to church station?
tool: get_muni_arrivals({"location":"Church Station"})
Claude: Here are the available directions at Church Station:

  Outbound (OB) - Lines K, L, M
  Toward: 47th Ave & Cutler Ave, Balboa Park BART, San Jose Ave & Niagara Ave

  Inbound (IB) - Lines K, L, M
  Toward: Metro Embarcadero Station (downtown)

Which direction are you heading?
You: outbound
tool: get_muni_arrivals({"location":"Church Station","direction":"OB"})
Claude: Here are the next Outbound departures from Church Station:

  [K] INGLESIDE → Balboa Park BART — 4 min
  [M] OCEAN VIEW → San Jose Ave & Niagara Ave — 6 min
  [L] TARAVAL → 47th Ave & Cutler Ave — 8 min
  [K] INGLESIDE → Balboa Park BART — 14 min
  [M] OCEAN VIEW → San Jose Ave & Niagara Ave — 17 min
```

It accounts for your travel time to the station too:

```
You: im 30 min walk from church station and i want to go to embarcadero. what train should i take?
tool: get_muni_arrivals({"location":"Church Station"})
tool: get_muni_arrivals({"location":"Church Station","direction":"IB"})
Claude: Take **K, L, or M** inbound - all go directly to Embarcadero.

Since you're 30 min away, catch one of these:
• **K** - 39 min from now
• **L** - 43 min from now
• **M** - 45 min from now

All are direct - no transfers needed.
```

## Setup

1. Get an [Anthropic API key](https://console.anthropic.com/) and a [511.org API key](https://511.org/open-data/token).

2. Set environment variables:
   ```sh
   export ANTHROPIC_API_KEY="your-key"
   export MUNI_API_KEY="your-511-key"
   ```

3. Run it:
   ```sh
   go run .
   ```

## How it works

The agent skeleton (chat loop, tool dispatch, inference) comes from Thorsten Ball's tutorial [How to Build an Agent](https://ampcode.com/notes/how-to-build-an-agent). On top of that, a `get_muni_arrivals` tool provides real-time transit data via the [511.org API](https://511.org/open-data):

- **Stop lookup** — searches all SF Muni stops by name, or geocodes a street address and finds the nearest stop.
- **Direction filtering** — called without a direction, it returns the available directions (IB/OB) and their lines. Called with a direction, it returns upcoming departures. Infers direction from context when possible.
- **Travel time aware** — if you mention how far you are from the station, it filters out trains you can't catch.
- **Real-time data** — uses the SIRI StopMonitoring endpoint for live predictions, marked with a `LIVE` indicator when GPS-based.

## Project structure

| File | Purpose |
|------|---------|
| `main.go` | Agent loop, tool dispatch, Claude API calls |
| `muni.go` | Muni tool: stop search, geocoding, 511 API integration |
