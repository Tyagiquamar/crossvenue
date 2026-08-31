# Execution Model (paper)

Execution is always simulated. There is no live-order code path.

## Order types

- `Market` — walk available depth until filled or depth exhausted.
- `IOCLimit` — walk depth up to the limit price; cancel any remainder.

## Order state machine

```
Pending -> Accepted -> PartiallyFilled -> Filled
   |          |              |
   |          v              v
   |      Cancelled      Cancelled (IOC remainder / cancel)
   v
 Rejected / Expired
```

Illegal transitions (e.g. `Filled -> Accepted`) are rejected with
`ErrIllegalTransition`. Terminal states: Filled, Cancelled, Rejected,
Expired.

## Depth consumption and partial fills

Fills consume observable depth from the current book snapshot. The
simulator does **not** mutate the book (our paper order does not move the
market) and never invents liquidity: if depth is insufficient, a Market
order partially fills and an IOC order cancels the remainder. Fill VWAP is
computed over the consumed levels.

## Two-leg arbitrage is not atomic

`ExecuteArbitrage` fills each leg against its venue independently. If the
sell leg fills less than the buy leg, the difference is surfaced as
`ResidualQty` (directional exposure), journaled as `PositionChanged`, and
visible to the risk engine. Policies: `parallel` (default, both legs
against the same snapshot instant), `buy-first`, `sell-first`.

Real exchanges provide no cross-venue atomicity; this model reflects that.

## Fees

Per-venue configurable taker/maker basis points (`venues.<v>.taker_bps`).
Fees are charged per fill and included in settlement. Fee values in
config are simulation parameters, not claims about current exchange fees.

## Latency model

Configurable per-venue outbound latency plus decision latency, converted
into a money penalty against notional (`PenaltyBpsPerMs`). Modes:
`instant`, `fixed`, `sampled` (sampled uses a seeded deterministic RNG in
replay). Modeled latency is not measured exchange latency.

## Idempotency

`Submit` is keyed on `ClientOrderID`: resubmission returns the original
order. Verified under concurrency in `execution_test.go`.
