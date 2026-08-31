# Replay Contract

## Guarantee

Given identical:

- recording file
- configuration
- random seed
- engine version

the following are identical across runs:

- normalized event ordering
- final book digests (per venue+symbol)
- opportunity stream (IDs and order)
- order state transitions
- final balances and positions
- realized PnL
- journal digest

## How it is achieved

- Recording: versioned newline-JSON envelope (`RecordedEvent`, version 1).
  Envelope semantics are encoding-agnostic; a length-prefixed binary writer
  can replace the transport without changing the model.
- Replay mode runs the **same** pipeline, opportunity engine, execution
  simulator, risk engine, and ledger as live mode — no separate logic.
- Replay runs deterministically: events are applied synchronously in file
  order and evaluation runs per event. No wall-clock timers participate.
- Decision time comes from the injected `clock.Clock`; replay tooling pins
  a `ManualClock`.
- Randomness (synthetic feeds, sampled latency) uses seeded generators only.
- Parity is enforced by `tests/replay/parity_test.go` (same fixture run
  twice must produce identical digests) and by CI, which hashes two full
  `cmd/replay` runs of the same generated recording.

## Speed modes

- `--speed 1x` preserves recorded receive-time spacing.
- `--speed Nx` divides spacing by N.
- `--speed max` streams as fast as possible (used by parity tests).

## Book digest

FNV-1a over sorted levels and sequence per book; printed by `cmd/replay`
as `book <venue|symbol> digest <hex>` plus a `journal digest` line.
