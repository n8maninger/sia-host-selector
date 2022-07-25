# Sia Host Selector

Selects hosts for renting based on hardcoded criteria using Sia Central's host
API.

# Building

```sh
go build -o bin/ ./cmd/selectord
```

# Running
```
SIA_API_ADDRESS=localhost:9980 ./bin/selectord
```