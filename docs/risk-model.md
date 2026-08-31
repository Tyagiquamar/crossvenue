# Risk Model

Every simulated order passes pre-trade checks before submission. The
decision is deterministic: same inputs, same decision.

## Checks (order-stable)

| Check | Config | Effect |
|---|---|---|
| Kill switch | `risk.kill_switch` / API | Block all new orders |
| Outstanding orders | `max_outstanding_orders` | Block when at limit |
| Consecutive failures | `max_consecutive_failures` | Block; auto kill switch |
| Trade notional | `max_trade_notional_usd` | Block oversized trades |
| Expected edge | `min_profit_usd` | Block sub-threshold |
| Daily loss | `daily_loss_limit_usd` | Block at realized loss limit |
| Book readiness | — | Block if either book not ready |
| Book staleness | `max_quote_age_ms` | Block stale books |
| Quote age | `max_quote_age_ms` | Block old quotes |
| Venue exposure | `max_venue_exposure_usd` | Block concentrated exposure |
| Total exposure | `max_total_exposure_usd` | Block aggregate exposure |
| Inventory | per-venue balances | Block when quote/base unavailable |

## Inventory

Assets do not move between exchanges. A cross-venue trade requires quote
currency on the buy venue and base inventory on the sell venue. Trades
that cannot be settled are rejected with `insufficient_*_inventory`.

## Kill switch

- Activation: API (`POST /api/v1/kill-switch`), config, or automatic on
  `max_consecutive_failures`.
- Effect: immediate block of all new submissions.
- Journal: `KillSwitchActivated` with reason; reset via
  `POST /api/v1/kill-switch/reset`.
- Metric: risk rejections increment.

## Exposure

Positions are tracked per venue+symbol in base units; notional exposure
uses the venue mid price. Residual exposure from non-atomic two-leg fills
is a first-class position and counts toward limits.
