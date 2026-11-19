package agent

// TODO: the event types the reducer below consumes — one agent run's
// history as a sequence of these. No kernel, tool, or model imports here;
// this package stays pure and gets golden-tested without touching
// Postgres, Temporal, or any LLM.
